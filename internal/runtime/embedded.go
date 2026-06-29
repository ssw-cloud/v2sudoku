package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sudokuapp "github.com/SUDOKU-ASCII/sudoku/internal/app"
	sudokuconfig "github.com/SUDOKU-ASCII/sudoku/internal/config"
	"github.com/SUDOKU-ASCII/sudoku/internal/handler"
	"github.com/SUDOKU-ASCII/sudoku/internal/protocol"
	"github.com/SUDOKU-ASCII/sudoku/internal/tunnel"
	"github.com/SUDOKU-ASCII/sudoku/pkg/connutil"
	"github.com/SUDOKU-ASCII/sudoku/pkg/obfs/httpmask"
	"github.com/SUDOKU-ASCII/sudoku/pkg/obfs/sudoku"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
	log "github.com/sirupsen/logrus"
)

type Embedded struct {
	node     *panel.NodeInfo
	runtime  conf.RuntimeConfig
	keyStore *ClientKeyStore

	mu             sync.RWMutex
	users          map[string]panel.UserInfo
	userHashToUUID map[string]string
	counters       map[string]*trafficCounter
	online         map[string]onlineState
	aliveList      map[int]int

	cfg       *sudokuconfig.Config
	tables    []*sudoku.Table
	listener  net.Listener
	activeMu  sync.Mutex
	active    map[net.Conn]struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewEmbedded(node *panel.NodeInfo, users []panel.UserInfo, alive map[int]int, cfg conf.RuntimeConfig, keyStore *ClientKeyStore) (*Embedded, error) {
	sc, err := buildSudokuConfig(node, cfg)
	if err != nil {
		return nil, err
	}
	tables, err := sudokuapp.BuildTables(sc)
	if err != nil {
		return nil, fmt.Errorf("build sudoku tables: %w", err)
	}
	e := &Embedded{
		node:           node,
		runtime:        cfg,
		keyStore:       keyStore,
		users:          map[string]panel.UserInfo{},
		userHashToUUID: map[string]string{},
		counters:       map[string]*trafficCounter{},
		online:         map[string]onlineState{},
		aliveList:      cloneAliveList(alive),
		cfg:            sc,
		tables:         tables,
		active:         map[net.Conn]struct{}{},
	}
	if err := e.replaceUsers(users); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Embedded) Start() error {
	addr := listenAddress(e.node)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	e.listener = ln
	e.wg.Add(1)
	go e.acceptLoop()
	log.WithFields(log.Fields{
		"node_id": e.node.Id,
		"addr":    addr,
	}).Info("embedded sudoku listener started")
	return nil
}

func (e *Embedded) Close() error {
	var err error
	e.closeOnce.Do(func() {
		if e.listener != nil {
			err = e.listener.Close()
		}
		e.closeActiveConns()
		e.wg.Wait()
	})
	return err
}

func (e *Embedded) SetAliveList(alive map[int]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aliveList = cloneAliveList(alive)
}

func (e *Embedded) UpdateUsers(added, deleted, modified, full []panel.UserInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if full != nil {
		return e.replaceUsersLocked(full)
	}
	for _, user := range deleted {
		e.deleteUserLocked(user)
	}
	for _, user := range added {
		e.upsertUserLocked(user)
	}
	for _, user := range modified {
		e.upsertUserLocked(user)
	}
	return e.rebuildUserHashesLocked()
}

func (e *Embedded) GetUserTrafficSlice(reportMin int) []panel.UserTraffic {
	e.mu.RLock()
	defer e.mu.RUnlock()
	threshold := int64(reportMin) * 1000
	out := make([]panel.UserTraffic, 0, len(e.counters))
	for uuid, counter := range e.counters {
		up, down := counter.snapshot()
		if up+down <= threshold {
			continue
		}
		user, ok := e.users[uuid]
		if !ok || user.Id == 0 {
			continue
		}
		out = append(out, panel.UserTraffic{
			UID:      user.Id,
			UUID:     uuid,
			Upload:   up,
			Download: down,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out
}

func (e *Embedded) ConfirmUserTraffic(reported []panel.UserTraffic) {
	if len(reported) == 0 {
		return
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, traffic := range reported {
		uuid := traffic.UUID
		if uuid == "" {
			uuid = e.uuidByUIDLocked(traffic.UID)
		}
		counter := e.counters[uuid]
		if counter == nil {
			continue
		}
		counter.subtract(traffic.Upload, traffic.Download)
	}
}

func (e *Embedded) GetOnlineDevice() []panel.OnlineUser {
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
		user, ok := e.users[uuid]
		if !ok || user.Id == 0 {
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

func (e *Embedded) acceptLoop() {
	defer e.wg.Done()
	for {
		conn, err := e.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.WithFields(log.Fields{
				"node_id": e.node.Id,
				"err":     err,
			}).Warn("accept failed")
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.registerConn(conn)
			defer e.unregisterConn(conn)
			e.handleConn(conn)
		}()
	}
}

func (e *Embedded) registerConn(conn net.Conn) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	e.active[conn] = struct{}{}
}

func (e *Embedded) unregisterConn(conn net.Conn) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	delete(e.active, conn)
	_ = conn.Close()
}

func (e *Embedded) closeActiveConns() {
	e.activeMu.Lock()
	conns := make([]net.Conn, 0, len(e.active))
	for conn := range e.active {
		conns = append(conns, conn)
	}
	e.activeMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (e *Embedded) handleConn(rawConn net.Conn) {
	var tunnelSrv *httpmask.TunnelServer
	e.mu.RLock()
	cfg := *e.cfg
	tables := append([]*sudoku.Table(nil), e.tables...)
	e.mu.RUnlock()

	if cfg.HTTPMaskTunnelEnabled() {
		tunnelSrv = httpmask.NewTunnelServer(httpmask.TunnelServerOptions{
			Mode:     cfg.HTTPMask.Mode,
			PathRoot: cfg.HTTPMask.PathRoot,
			AuthKey:  cfg.Key,
			EarlyHandshake: tunnel.NewHTTPMaskServerEarlyHandshake(tunnel.EarlyCodecConfig{
				PSK:                cfg.Key,
				AEAD:               cfg.AEAD,
				EnablePureDownlink: cfg.EnablePureDownlink,
				PaddingMin:         cfg.PaddingMin,
				PaddingMax:         cfg.PaddingMax,
			}, tables, tunnel.AllowHandshakeReplay),
			PassThroughOnReject: func() bool {
				if cfg.SuspiciousAction == "silent" {
					return true
				}
				return cfg.SuspiciousAction == "fallback" && strings.TrimSpace(cfg.FallbackAddr) != ""
			}(),
		})
	}

	if tunnelSrv != nil {
		res, c, err := tunnelSrv.HandleConn(rawConn)
		if err != nil {
			log.WithError(err).Warn("sudoku httpmask prelude failed")
			_ = rawConn.Close()
			return
		}
		switch res {
		case httpmask.HandleDone:
			return
		case httpmask.HandleStartTunnel:
			inner := cfg
			inner.HTTPMask.Disable = true
			e.handleSudokuConn(c, rawConn, &inner, tables, false)
			return
		case httpmask.HandlePassThrough:
			if r, ok := c.(interface{ IsHTTPMaskRejected() bool }); ok && r.IsHTTPMaskRejected() {
				handler.HandleSuspicious(c, rawConn, &cfg)
				return
			}
			e.handleSudokuConn(c, rawConn, &cfg, tables, true)
			return
		default:
			_ = rawConn.Close()
			return
		}
	}
	e.handleSudokuConn(rawConn, rawConn, &cfg, tables, true)
}

func (e *Embedded) handleSudokuConn(handshakeConn net.Conn, rawConn net.Conn, cfg *sudokuconfig.Config, tables []*sudoku.Table, allowFallback bool) {
	tunnelConn, meta, err := tunnel.HandshakeAndUpgradeWithTablesMeta(handshakeConn, cfg, tables)
	if err != nil {
		if suspErr, ok := err.(*tunnel.SuspiciousError); ok {
			log.WithFields(log.Fields{
				"node_id": e.node.Id,
				"err":     suspErr.Err,
			}).Warn("suspicious sudoku connection")
			if allowFallback {
				handler.HandleSuspicious(suspErr.Conn, handshakeConn, cfg)
			} else {
				_ = rawConn.Close()
			}
			return
		}
		log.WithFields(log.Fields{
			"node_id": e.node.Id,
			"err":     err,
		}).Warn("sudoku handshake failed")
		_ = rawConn.Close()
		return
	}

	userHash := ""
	if meta != nil {
		userHash = meta.UserHash
	}
	uuid, counter, ok := e.counterForUserHash(userHash, rawConn.RemoteAddr())
	if !ok {
		log.WithFields(log.Fields{
			"node_id":   e.node.Id,
			"user_hash": userHash,
		}).Warn("reject sudoku connection with unknown user hash")
		_ = tunnelConn.Close()
		return
	}
	counted := &countingConn{Conn: tunnelConn, counter: counter}

	msg, err := readFirstControl(counted)
	if err != nil {
		log.WithFields(log.Fields{
			"node_id": e.node.Id,
			"uid":     counter.uid,
			"err":     err,
		}).Warn("read sudoku control message failed")
		return
	}

	switch msg.Type {
	case tunnel.KIPTypeStartUoT:
		logAccess(e.node.Id, counter.uid, clientIP(addrString(rawConn.RemoteAddr())), "uot", "session")
		if err := e.handleUoT(counted); err != nil && !isExpectedClose(err) {
			log.WithError(err).Debug("sudoku uot ended")
		}
	case tunnel.KIPTypeStartMux:
		logAccess(e.node.Id, counter.uid, clientIP(addrString(rawConn.RemoteAddr())), "mux", "session")
		if err := tunnel.HandleMuxWithDialer(counted, func(addr string) {
			logAccess(e.node.Id, counter.uid, clientIP(addrString(rawConn.RemoteAddr())), "mux", addr)
		}, func(addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 10*time.Second)
		}); err != nil && !isExpectedClose(err) {
			log.WithError(err).Debug("sudoku mux ended")
		}
	case tunnel.KIPTypeOpenTCP:
		destAddrStr, _, _, err := protocol.ReadAddress(bytes.NewReader(msg.Payload))
		if err != nil {
			log.WithError(err).Warn("decode sudoku target address failed")
			return
		}
		logAccess(e.node.Id, counter.uid, clientIP(addrString(rawConn.RemoteAddr())), "tcp", destAddrStr)
		target, err := net.DialTimeout("tcp", destAddrStr, 10*time.Second)
		if err != nil {
			log.WithError(err).Warn("connect sudoku target failed")
			return
		}
		pipeConn(counted, target)
	default:
		log.WithFields(log.Fields{
			"node_id": e.node.Id,
			"uid":     counter.uid,
			"type":    msg.Type,
		}).Warn("unknown sudoku control message")
	}

	e.markOffline(uuid)
}

func readFirstControl(conn net.Conn) (*tunnel.KIPMessage, error) {
	for {
		msg, err := tunnel.ReadKIPMessage(conn)
		if err != nil {
			return nil, err
		}
		if msg.Type == tunnel.KIPTypeKeepAlive {
			continue
		}
		return msg, nil
	}
}

func (e *Embedded) handleUoT(conn net.Conn) error {
	pConn, err := net.ListenPacket("udp", "")
	if err != nil {
		return fmt.Errorf("listen udp for uot: %w", err)
	}
	defer pConn.Close()

	errCh := make(chan error, 1)
	var once sync.Once
	closeAll := func(err error) {
		once.Do(func() {
			_ = conn.Close()
			_ = pConn.Close()
			errCh <- err
		})
	}

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := pConn.ReadFrom(buf)
			if err != nil {
				closeAll(err)
				return
			}
			if err := tunnel.WriteUoTDatagram(conn, addr.String(), buf[:n]); err != nil {
				closeAll(err)
				return
			}
		}
	}()

	go func() {
		for {
			addrStr, payload, err := tunnel.ReadUoTDatagram(conn)
			if err != nil {
				closeAll(err)
				return
			}
			udpAddr, err := net.ResolveUDPAddr("udp", addrStr)
			if err != nil {
				continue
			}
			if _, err := pConn.WriteTo(payload, udpAddr); err != nil {
				closeAll(err)
				return
			}
		}
	}()

	return <-errCh
}

func (e *Embedded) replaceUsers(users []panel.UserInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replaceUsersLocked(users)
}

func (e *Embedded) replaceUsersLocked(users []panel.UserInfo) error {
	e.users = make(map[string]panel.UserInfo, len(users))
	for _, user := range users {
		if user.Uuid == "" {
			continue
		}
		e.users[user.Uuid] = user
		if counter := e.counters[user.Uuid]; counter != nil {
			counter.uid = user.Id
			counter.uuid = user.Uuid
		} else {
			e.counters[user.Uuid] = &trafficCounter{uid: user.Id, uuid: user.Uuid}
		}
	}
	for uuid := range e.counters {
		if _, ok := e.users[uuid]; !ok {
			if up, down := e.counters[uuid].snapshot(); up == 0 && down == 0 {
				delete(e.counters, uuid)
			}
		}
	}
	return e.rebuildUserHashesLocked()
}

func (e *Embedded) upsertUserLocked(user panel.UserInfo) {
	if user.Uuid == "" {
		return
	}
	e.users[user.Uuid] = user
	counter := e.counters[user.Uuid]
	if counter == nil {
		counter = &trafficCounter{}
		e.counters[user.Uuid] = counter
	}
	counter.uid = user.Id
	counter.uuid = user.Uuid
}

func (e *Embedded) deleteUserLocked(user panel.UserInfo) {
	delete(e.users, user.Uuid)
	delete(e.online, user.Uuid)
	if counter := e.counters[user.Uuid]; counter != nil {
		if up, down := counter.snapshot(); up == 0 && down == 0 {
			delete(e.counters, user.Uuid)
		}
	}
}

func (e *Embedded) rebuildUserHashesLocked() error {
	users := make([]panel.UserInfo, 0, len(e.users))
	for _, user := range e.users {
		users = append(users, user)
	}
	keys, err := e.keyStore.BuildMappings(e.node, users)
	if err != nil {
		return err
	}
	e.userHashToUUID = map[string]string{}
	for _, userKey := range keys {
		if userKey.UserHash != "" && userKey.UUID != "" {
			e.userHashToUUID[userKey.UserHash] = userKey.UUID
		}
	}
	return nil
}

func (e *Embedded) counterForUserHash(userHash string, remote net.Addr) (string, *trafficCounter, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	uuid := e.userHashToUUID[userHash]
	if uuid == "" {
		return "", nil, false
	}
	user := e.users[uuid]
	if user.Id == 0 {
		return "", nil, false
	}
	counter := e.counters[uuid]
	if counter == nil {
		counter = &trafficCounter{uid: user.Id, uuid: uuid}
		e.counters[uuid] = counter
	}
	counter.uid = user.Id
	counter.uuid = uuid
	e.online[uuid] = onlineState{
		seenAt: time.Now(),
		ip:     clientIP(addrString(remote)),
	}
	return uuid, counter, true
}

func (e *Embedded) markOffline(uuid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state, ok := e.online[uuid]; ok {
		state.seenAt = time.Now()
		e.online[uuid] = state
	}
}

func (e *Embedded) uuidByUIDLocked(uid int) string {
	for uuid, user := range e.users {
		if user.Id == uid {
			return uuid
		}
	}
	return ""
}

func cloneAliveList(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

func clientIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

func logAccess(nodeID int, uid int, source string, network string, target string) {
	if target == "" {
		target = "-"
	}
	log.Infof("| node:%d | from %s |accepted| %s:%s | target:%s | user_id:%s",
		nodeID,
		source,
		network,
		target,
		target,
		strconv.Itoa(uid),
	)
}

func pipeConn(a net.Conn, b net.Conn) {
	defer a.Close()
	defer b.Close()
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(a, b)
		_ = connutil.TryCloseWrite(a)
		_ = connutil.TryCloseRead(b)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(b, a)
		_ = connutil.TryCloseWrite(b)
		_ = connutil.TryCloseRead(a)
		errCh <- err
	}()
	<-errCh
}

func isExpectedClose(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}

func (e *Embedded) DebugUserHashMap() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string]int{}
	for hash, uuid := range e.userHashToUUID {
		out[hash] = e.users[uuid].Id
	}
	return out
}

func (e *Embedded) MarshalConfig() ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return json.MarshalIndent(e.cfg, "", "  ")
}
