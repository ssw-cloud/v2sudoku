package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
	log "github.com/sirupsen/logrus"
)

type External struct {
	node       *panel.NodeInfo
	runtime    conf.RuntimeConfig
	workDir    string
	configPath string

	mu             sync.Mutex
	users          map[string]panel.UserInfo
	userHashToUUID map[string]string
	pending        map[string]pendingTraffic
	online         map[string]onlineState
	aliveList      map[int]int
	lastConfigHash string
	cmd            *exec.Cmd
	keyStore       *ClientKeyStore
}

type pendingTraffic struct {
	UID      int
	UUID     string
	Upload   int64
	Download int64
}

func NewExternal(node *panel.NodeInfo, users []panel.UserInfo, alive map[int]int, cfg conf.RuntimeConfig, keyStore *ClientKeyStore) (*External, error) {
	workDir := filepath.Join(cfg.WorkingDir, fmt.Sprintf("node-%d", node.Id))
	e := &External{
		node:           node,
		runtime:        cfg,
		workDir:        workDir,
		configPath:     filepath.Join(workDir, "server.config.json"),
		users:          map[string]panel.UserInfo{},
		userHashToUUID: map[string]string{},
		pending:        map[string]pendingTraffic{},
		online:         map[string]onlineState{},
		aliveList:      cloneAliveList(alive),
		keyStore:       keyStore,
	}
	if err := e.replaceUsersLocked(users); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *External) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(e.workDir, 0755); err != nil {
		return err
	}
	if err := e.applyConfigLocked(); err != nil {
		return err
	}
	return e.startLocked()
}

func (e *External) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopLocked()
}

func (e *External) SetAliveList(alive map[int]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aliveList = cloneAliveList(alive)
}

func (e *External) UpdateUsers(added, deleted, modified, full []panel.UserInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if full != nil {
		if err := e.replaceUsersLocked(full); err != nil {
			return err
		}
	} else {
		for _, user := range deleted {
			delete(e.users, user.Uuid)
			delete(e.online, user.Uuid)
		}
		for _, user := range added {
			if user.Uuid != "" {
				e.users[user.Uuid] = user
			}
		}
		for _, user := range modified {
			if user.Uuid != "" {
				e.users[user.Uuid] = user
			}
		}
		if err := e.rebuildUserHashesLocked(); err != nil {
			return err
		}
	}
	return e.applyConfigLocked()
}

func (e *External) GetUserTrafficSlice(reportMin int) []panel.UserTraffic {
	e.mu.Lock()
	defer e.mu.Unlock()
	threshold := int64(reportMin) * 1000
	out := make([]panel.UserTraffic, 0, len(e.pending))
	for _, delta := range e.pending {
		if delta.Upload+delta.Download <= threshold {
			continue
		}
		out = append(out, panel.UserTraffic{
			UID:      delta.UID,
			UUID:     delta.UUID,
			Upload:   delta.Upload,
			Download: delta.Download,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out
}

func (e *External) ConfirmUserTraffic(reported []panel.UserTraffic) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, traffic := range reported {
		uuid := traffic.UUID
		if uuid == "" {
			uuid = e.uuidByUIDLocked(traffic.UID)
		}
		delta := e.pending[uuid]
		if traffic.Upload >= delta.Upload {
			delta.Upload = 0
		} else {
			delta.Upload -= traffic.Upload
		}
		if traffic.Download >= delta.Download {
			delta.Download = 0
		} else {
			delta.Download -= traffic.Download
		}
		if delta.Upload == 0 && delta.Download == 0 {
			delete(e.pending, uuid)
		} else {
			e.pending[uuid] = delta
		}
	}
}

func (e *External) GetOnlineDevice() []panel.OnlineUser {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-2 * e.node.PushInterval)
	if e.node.PushInterval <= 0 {
		cutoff = time.Now().Add(-2 * time.Minute)
	}
	out := make([]panel.OnlineUser, 0, len(e.online))
	for uuid, online := range e.online {
		if online.seenAt.Before(cutoff) {
			delete(e.online, uuid)
			continue
		}
		user := e.users[uuid]
		if user.Id == 0 {
			continue
		}
		ip := online.ip
		if ip == "" {
			ip = fmt.Sprintf("unknown_%d", e.node.Id)
		}
		out = append(out, panel.OnlineUser{UID: user.Id, IP: ip})
	}
	return out
}

func (e *External) replaceUsersLocked(users []panel.UserInfo) error {
	e.users = make(map[string]panel.UserInfo, len(users))
	for _, user := range users {
		if user.Uuid != "" {
			e.users[user.Uuid] = user
		}
	}
	return e.rebuildUserHashesLocked()
}

func (e *External) rebuildUserHashesLocked() error {
	users := make([]panel.UserInfo, 0, len(e.users))
	for _, user := range e.users {
		users = append(users, user)
	}
	keys, err := e.keyStore.BuildMappings(e.node, users)
	if err != nil {
		return err
	}
	e.userHashToUUID = map[string]string{}
	for _, key := range keys {
		e.userHashToUUID[key.UserHash] = key.UUID
	}
	log.WithFields(log.Fields{
		"node_id":           e.node.Id,
		"client_key_source": normalizedClientKeySource(e.runtime.ClientKeySource),
		"users":             len(e.users),
		"user_hashes":       len(e.userHashToUUID),
	}).Info("rebuilt sudoku user hash mappings")
	return nil
}

func (e *External) applyConfigLocked() error {
	body, err := buildExternalConfigJSON(e.node, e.runtime)
	if err != nil {
		return err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	if hash == e.lastConfigHash {
		return nil
	}
	if err := os.MkdirAll(e.workDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(e.configPath, append(body, '\n'), 0600); err != nil {
		return fmt.Errorf("write sudoku config: %w", err)
	}
	e.lastConfigHash = hash
	if e.cmd != nil && e.cmd.Process != nil {
		if err := e.restartLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (e *External) startLocked() error {
	if e.cmd != nil && e.cmd.Process != nil {
		return nil
	}
	cmd := exec.Command(e.runtime.SudokuPath, "-c", e.configPath)
	cmd.Dir = e.workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sudoku runtime: %w", err)
	}
	e.cmd = cmd
	go func() {
		err := cmd.Wait()
		if err != nil {
			log.WithFields(log.Fields{
				"node_id": e.node.Id,
				"err":     err,
			}).Warn("external sudoku runtime exited")
		}
		e.mu.Lock()
		if e.cmd == cmd {
			e.cmd = nil
		}
		e.mu.Unlock()
	}()
	log.WithFields(log.Fields{
		"node_id":           e.node.Id,
		"path":              e.runtime.SudokuPath,
		"config":            e.configPath,
		"client_key_source": normalizedClientKeySource(e.runtime.ClientKeySource),
		"user_hashes":       len(e.userHashToUUID),
	}).Info("external sudoku runtime started")
	return nil
}

func (e *External) restartLocked() error {
	if err := e.stopLocked(); err != nil {
		return err
	}
	return e.startLocked()
}

func (e *External) stopLocked() error {
	if e.cmd == nil || e.cmd.Process == nil {
		e.cmd = nil
		return nil
	}
	proc := e.cmd.Process
	_ = proc.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			if e.cmd == nil {
				close(done)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = proc.Kill()
	}
	e.cmd = nil
	return nil
}

func (e *External) uuidByUIDLocked(uid int) string {
	for uuid, user := range e.users {
		if user.Id == uid {
			return uuid
		}
	}
	return ""
}

func (e *External) ImportEventLine(raw string) bool {
	var event struct {
		Type     string `json:"type"`
		UserHash string `json:"user_hash"`
		IP       string `json:"ip"`
		Upload   int64  `json:"upload"`
		Download int64  `json:"download"`
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	uuid := e.userHashToUUID[event.UserHash]
	if uuid == "" {
		return true
	}
	user := e.users[uuid]
	if user.Id == 0 {
		return true
	}
	switch event.Type {
	case "open":
		e.online[uuid] = onlineState{seenAt: time.Now(), ip: event.IP}
	case "close":
		delete(e.online, uuid)
	case "traffic":
		delta := e.pending[uuid]
		delta.UID = user.Id
		delta.UUID = uuid
		delta.Upload += event.Upload
		delta.Download += event.Download
		e.pending[uuid] = delta
	}
	return true
}

func (e *External) ConfigPath() string {
	return e.configPath
}

var _ = context.Background
