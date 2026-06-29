package runtime

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	sudokuconfig "github.com/SUDOKU-ASCII/sudoku/internal/config"
	sudokucrypto "github.com/SUDOKU-ASCII/sudoku/pkg/crypto"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
)

type UserKey struct {
	UID      int
	UUID     string
	Key      string
	UserHash string
}

func buildSudokuConfig(node *panel.NodeInfo, cfg conf.RuntimeConfig) (*sudokuconfig.Config, error) {
	settings := node.EncryptionSettings
	key := strings.TrimSpace(settings.MasterPublicKey)
	if key == "" && strings.TrimSpace(settings.MasterPrivateKey) != "" {
		pub, err := sudokucrypto.RecoverPublicKey(settings.MasterPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("recover master public key: %w", err)
		}
		key = sudokucrypto.EncodePoint(pub)
	}
	if key == "" {
		return nil, fmt.Errorf("missing sudoku master public key")
	}

	httpMask := sudokuconfig.HTTPMaskConfig{
		Mode:      firstNonEmpty(settings.HTTPMask.Mode, "legacy"),
		Host:      settings.HTTPMask.Host,
		PathRoot:  settings.HTTPMask.PathRoot,
		Multiplex: firstNonEmpty(settings.HTTPMask.Multiplex, settings.Multiplex),
	}
	if settings.HTTPMask.Disable != nil {
		httpMask.Disable = *settings.HTTPMask.Disable
	}
	if settings.HTTPMask.TLS != nil {
		httpMask.TLS = *settings.HTTPMask.TLS
	}

	enablePureDownlink := true
	if settings.EnablePureDownlink != nil {
		enablePureDownlink = *settings.EnablePureDownlink
	}

	fallback := firstNonEmpty(settings.FallbackAddress, cfg.FallbackAddress)
	suspiciousAction := firstNonEmpty(settings.SuspiciousAction, "fallback")
	if fallback == "" {
		suspiciousAction = "silent"
	}

	sc := &sudokuconfig.Config{
		Mode:               "server",
		Transport:          "tcp",
		LocalPort:          node.ServerPort,
		FallbackAddr:       fallback,
		Key:                key,
		AEAD:               firstNonEmpty(settings.AEAD, "chacha20-poly1305"),
		SuspiciousAction:   suspiciousAction,
		PaddingMin:         firstNonZero(settings.PaddingMin, 5),
		PaddingMax:         firstNonZero(settings.PaddingMax, 15),
		ASCII:              firstNonEmpty(settings.ASCII, "prefer_entropy"),
		CustomTable:        settings.CustomTable,
		CustomTables:       append([]string(nil), settings.CustomTables...),
		EnablePureDownlink: enablePureDownlink,
		HTTPMask:           httpMask,
	}
	if err := sc.Finalize(); err != nil {
		return nil, err
	}
	return sc, nil
}

func buildExternalConfigJSON(node *panel.NodeInfo, cfg conf.RuntimeConfig) ([]byte, error) {
	sc, err := buildSudokuConfig(node, cfg)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(sc, "", "  ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func listenAddress(node *panel.NodeInfo) string {
	listenIP := node.ListenIP
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	return net.JoinHostPort(listenIP, strconv.Itoa(node.ServerPort))
}
