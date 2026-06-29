package runtime

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	sudokucrypto "github.com/SUDOKU-ASCII/sudoku/pkg/crypto"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
)

type ClientKeyStore struct {
	cfg  conf.RuntimeConfig
	mu   sync.Mutex
	file keyFileState
}

type keyFileState struct {
	Users map[string]string `json:"users"`
}

func NewClientKeyStore(cfg conf.RuntimeConfig) *ClientKeyStore {
	return &ClientKeyStore{cfg: cfg}
}

func (s *ClientKeyStore) BuildMappings(node *panel.NodeInfo, users []panel.UserInfo) ([]UserKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	source := strings.ToLower(strings.TrimSpace(s.cfg.ClientKeySource))
	if source == "" {
		source = "uuid"
	}
	if source == "auto" || source == "file" {
		if err := s.loadFileLocked(); err != nil {
			return nil, err
		}
	}

	out := make([]UserKey, 0, len(users))
	changed := false
	for _, user := range users {
		key, err := s.resolveKeyLocked(node, user, source)
		if err != nil {
			return nil, err
		}
		if key == "" {
			continue
		}
		userHash := userHashHex([]byte(key))
		out = append(out, UserKey{
			UID:      user.Id,
			UUID:     user.Uuid,
			Key:      key,
			UserHash: userHash,
		})
		if source == "auto" || source == "file" {
			if s.file.Users[strconv.Itoa(user.Id)] == "" {
				s.file.Users[strconv.Itoa(user.Id)] = key
				changed = true
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})

	if changed {
		if err := s.saveFileLocked(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ClientKeyStore) resolveKeyLocked(node *panel.NodeInfo, user panel.UserInfo, source string) (string, error) {
	switch source {
	case "deterministic_split", "split":
		return deterministicSplitKey(node.EncryptionSettings.MasterPrivateKey, node, user)
	case "uuid":
		return strings.TrimSpace(user.Uuid), nil
	case "deterministic":
		return deterministicUserKey(s.cfg.ClientKeySeed, node, user), nil
	case "file", "auto":
		key := strings.TrimSpace(s.file.Users[strconv.Itoa(user.Id)])
		if key != "" {
			return key, nil
		}
		if source == "file" {
			return "", nil
		}
		if len(s.file.Users) >= s.cfg.MaxClientKeyFileUsers {
			return "", fmt.Errorf("client key file reached limit %d", s.cfg.MaxClientKeyFileUsers)
		}
		if node.EncryptionSettings.MasterPrivateKey == "" {
			return "", fmt.Errorf("ClientKeySource=auto requires encryption_settings.master_private_key")
		}
		master, err := sudokucrypto.ParsePrivateScalar(node.EncryptionSettings.MasterPrivateKey)
		if err != nil {
			return "", fmt.Errorf("parse master private key: %w", err)
		}
		return sudokucrypto.SplitPrivateKey(master)
	default:
		return "", fmt.Errorf("unsupported ClientKeySource %q", source)
	}
}

func deterministicSplitKey(masterHex string, node *panel.NodeInfo, user panel.UserInfo) (string, error) {
	masterHex = strings.TrimSpace(masterHex)
	if masterHex == "" {
		return "", fmt.Errorf("ClientKeySource=deterministic_split requires encryption_settings.master_private_key")
	}
	masterBytes, err := hex.DecodeString(masterHex)
	if err != nil {
		return "", fmt.Errorf("decode master private key: %w", err)
	}
	if len(masterBytes) != 32 {
		return "", fmt.Errorf("master private key must be 32 bytes")
	}
	master := littleEndianScalar(masterBytes)
	seed := fmt.Sprintf("v2sudoku|node:%d|uid:%d|uuid:%s|master:%s", node.Id, user.Id, user.Uuid, masterHex)
	digest := sha512.Sum512([]byte(seed))
	r := littleEndianScalar(digest[:])
	r.Mod(r, ed25519Order())
	k := new(big.Int).Sub(master, r)
	k.Mod(k, ed25519Order())
	out := append(scalarLittleEndian(r), scalarLittleEndian(k)...)
	return hex.EncodeToString(out), nil
}

func littleEndianScalar(in []byte) *big.Int {
	buf := make([]byte, len(in))
	for i := range in {
		buf[len(in)-1-i] = in[i]
	}
	return new(big.Int).SetBytes(buf)
}

func scalarLittleEndian(v *big.Int) []byte {
	bigEndian := v.Bytes()
	out := make([]byte, 32)
	for i := 0; i < len(bigEndian) && i < 32; i++ {
		out[i] = bigEndian[len(bigEndian)-1-i]
	}
	return out
}

func ed25519Order() *big.Int {
	order, _ := new(big.Int).SetString("1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed", 16)
	return order
}

func (s *ClientKeyStore) loadFileLocked() error {
	if s.file.Users != nil {
		return nil
	}
	s.file.Users = map[string]string{}
	body, err := os.ReadFile(s.cfg.ClientKeyFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read client key file: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, &s.file); err != nil {
		return fmt.Errorf("decode client key file: %w", err)
	}
	if s.file.Users == nil {
		s.file.Users = map[string]string{}
	}
	return nil
}

func (s *ClientKeyStore) saveFileLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.ClientKeyFile), 0755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfg.ClientKeyFile + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.ClientKeyFile)
}

func deterministicUserKey(seed string, node *panel.NodeInfo, user panel.UserInfo) string {
	if seed == "" {
		seed = "v2sudoku"
	}
	raw := fmt.Sprintf("%s|node:%d|uid:%d|uuid:%s", seed, node.Id, user.Id, user.Uuid)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func userHashHex(privateKey []byte) string {
	sum := sha256.Sum256(privateKey)
	return hex.EncodeToString(sum[:8])
}
