package runtime

import (
	"encoding/hex"
	"encoding/json"
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

func TestBuildSudokuConfigAcceptsMetaCubeXFieldNames(t *testing.T) {
	pair, err := sudokucrypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
		"master_private_key": "` + sudokucrypto.EncodeScalar(pair.Private) + `",
		"aead-method": "aes-128-gcm",
		"table-type": "up_ascii_down_entropy",
		"padding-min": 2,
		"padding-max": 7,
		"custom-table": "xpxvvpvv",
		"custom-tables": ["xpxvvpvv", "vxpvxvvp"],
		"httpmask": {
			"mode": "stream",
			"tls": true,
			"mask-host": "cdn.example.com",
			"path-root": "aabbcc",
			"multiplex": "auto"
		},
		"enable-pure-downlink": false
	}`)
	var settings panel.SudokuSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	node := &panel.NodeInfo{
		Id:                 1,
		Protocol:           "sudoku",
		Host:               "example.com",
		ServerPort:         18080,
		EncryptionSettings: settings,
	}
	cfg, err := buildSudokuConfig(node, conf.New().RuntimeConfig)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	if cfg.AEAD != "aes-128-gcm" {
		t.Fatalf("AEAD = %q", cfg.AEAD)
	}
	if cfg.ASCII != "up_ascii_down_entropy" {
		t.Fatalf("ASCII = %q", cfg.ASCII)
	}
	if cfg.PaddingMin != 2 || cfg.PaddingMax != 7 {
		t.Fatalf("padding = %d/%d", cfg.PaddingMin, cfg.PaddingMax)
	}
	if cfg.CustomTable != "xpxvvpvv" || len(cfg.CustomTables) != 2 {
		t.Fatalf("custom tables not propagated")
	}
	if cfg.HTTPMask.Mode != "stream" || !cfg.HTTPMask.TLS || cfg.HTTPMask.Host != "cdn.example.com" || cfg.HTTPMask.PathRoot != "aabbcc" || cfg.HTTPMask.Multiplex != "auto" {
		t.Fatalf("httpmask not propagated: %+v", cfg.HTTPMask)
	}
	if cfg.EnablePureDownlink {
		t.Fatalf("EnablePureDownlink should be false")
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
