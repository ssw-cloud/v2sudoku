package runtime

import (
	"encoding/hex"
	"testing"

	sudokucrypto "github.com/SUDOKU-ASCII/sudoku/pkg/crypto"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
)

func TestBuildSudokuConfigRecoversPublicKey(t *testing.T) {
	pair, err := sudokucrypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	node := &panel.NodeInfo{
		Id:         1,
		Protocol:   "sudoku",
		Host:       "example.com",
		ListenIP:   "127.0.0.1",
		ServerPort: 18080,
		EncryptionSettings: panel.SudokuSettings{
			MasterPrivateKey: sudokucrypto.EncodeScalar(pair.Private),
		},
	}
	cfg, err := buildSudokuConfig(node, conf.New().RuntimeConfig)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	if cfg.Key != sudokucrypto.EncodePoint(pair.Public) {
		t.Fatalf("unexpected public key")
	}
	if cfg.LocalPort != 18080 {
		t.Fatalf("unexpected port %d", cfg.LocalPort)
	}
}

func TestClientKeyStoreUUIDMapping(t *testing.T) {
	node := &panel.NodeInfo{Id: 7}
	users := []panel.UserInfo{{Id: 42, Uuid: "user-key"}}
	cfg := conf.New().RuntimeConfig
	cfg.ClientKeySource = "uuid"
	store := NewClientKeyStore(cfg)
	mappings, err := store.BuildMappings(node, users)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings", len(mappings))
	}
	if mappings[0].Key != "user-key" {
		t.Fatalf("unexpected key %q", mappings[0].Key)
	}
	if _, err := hex.DecodeString(mappings[0].UserHash); err != nil {
		t.Fatalf("invalid user hash: %v", err)
	}
}

func TestDeterministicSplitKeyRecoversMasterPublicKey(t *testing.T) {
	pair, err := sudokucrypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	node := &panel.NodeInfo{
		Id: 99,
		EncryptionSettings: panel.SudokuSettings{
			MasterPrivateKey: sudokucrypto.EncodeScalar(pair.Private),
		},
	}
	user := panel.UserInfo{Id: 123, Uuid: "user-uuid"}
	key, err := deterministicSplitKey(node.EncryptionSettings.MasterPrivateKey, node, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 128 {
		t.Fatalf("split key length = %d", len(key))
	}
	pub, err := sudokucrypto.RecoverPublicKey(key)
	if err != nil {
		t.Fatalf("recover public key: %v", err)
	}
	if sudokucrypto.EncodePoint(pub) != sudokucrypto.EncodePoint(pair.Public) {
		t.Fatalf("split key does not recover master public key")
	}
}
