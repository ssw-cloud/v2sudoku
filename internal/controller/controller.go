package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/runtime"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/task"
	log "github.com/sirupsen/logrus"
)

type Controller struct {
	apiClient      *panel.Client
	conf           *conf.NodeConfig
	info           *panel.NodeInfo
	server         runtime.Server
	tag            string
	userList       []panel.UserInfo
	reloadCh       chan struct{}
	runtime        conf.RuntimeConfig
	keyStore       *runtime.ClientKeyStore
	nodeInfoTask   *task.Task
	userReportTask *task.Task
	aliveList      map[int]int
}

type panelSnapshot struct {
	APIHost string           `json:"api_host"`
	NodeID  int              `json:"node_id"`
	Node    *panel.NodeInfo  `json:"node"`
	Users   []panel.UserInfo `json:"users"`
	Alive   map[int]int      `json:"alive"`
	SavedAt time.Time        `json:"saved_at"`
}

func New(api *panel.Client, nodeConf *conf.NodeConfig, runtimeConfig conf.RuntimeConfig, keyStore *runtime.ClientKeyStore, reloadCh chan struct{}) *Controller {
	return &Controller{
		apiClient: api,
		conf:      nodeConf,
		runtime:   runtimeConfig,
		keyStore:  keyStore,
		reloadCh:  reloadCh,
	}
}

func (c *Controller) Start() error {
	snapshot, err := c.startupPanelSnapshot()
	if err != nil {
		return err
	}
	node := snapshot.Node
	users := cloneUserList(snapshot.Users)
	aliveMap := cloneAliveList(snapshot.Alive)

	c.info = node
	c.userList = users
	c.aliveList = cloneAliveList(aliveMap)
	c.tag = node.Tag

	server, err := c.newServer(node, users, aliveMap)
	if err != nil {
		return err
	}
	c.server = server
	if err := c.server.Start(); err != nil {
		return fmt.Errorf("start v2sudoku server error: %w", err)
	}
	c.startTasks()
	return nil
}

func (c *Controller) Close() {
	if c.nodeInfoTask != nil {
		c.nodeInfoTask.Close()
	}
	if c.userReportTask != nil {
		c.userReportTask.Close()
	}
	if c.server != nil {
		_ = c.server.Close()
	}
}

func (c *Controller) newServer(node *panel.NodeInfo, users []panel.UserInfo, alive map[int]int) (runtime.Server, error) {
	switch c.runtime.EngineName() {
	case conf.EngineExternal:
		return runtime.NewExternal(node, users, alive, c.runtime, c.keyStore)
	default:
		return runtime.NewEmbedded(node, users, alive, c.runtime, c.keyStore)
	}
}

func (c *Controller) startTasks() {
	c.nodeInfoTask = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: c.info.PullInterval,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: c.reloadCh,
	}
	c.userReportTask = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: c.info.PushInterval,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: c.reloadCh,
	}
	_ = c.nodeInfoTask.Start(false)
	_ = c.userReportTask.Start(false)
}

func (c *Controller) startupPanelSnapshot() (*panelSnapshot, error) {
	snapshot, err := c.fetchPanelSnapshot(context.Background())
	if err == nil {
		if err := c.savePanelSnapshot(snapshot); err != nil {
			log.WithError(err).Warn("save panel cache failed")
		}
		return snapshot, nil
	}
	if panel.IsAuthStatusError(err) {
		return nil, err
	}

	cached, cacheErr := c.readPanelSnapshot()
	if cacheErr != nil {
		return nil, fmt.Errorf("fetch panel snapshot failed: %w; cached snapshot unavailable: %v", err, cacheErr)
	}
	log.WithFields(log.Fields{
		"node_id":  c.conf.NodeID,
		"saved_at": cached.SavedAt.Format(time.RFC3339),
		"err":      err,
	}).Warn("panel unavailable; starting with cached snapshot")
	return cached, nil
}

func (c *Controller) fetchPanelSnapshot(ctx context.Context) (*panelSnapshot, error) {
	node, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("get node info error: %w", err)
	}
	if node == nil {
		return nil, fmt.Errorf("empty node info")
	}
	users, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user list error: %w", err)
	}
	if users == nil {
		return nil, fmt.Errorf("empty user list")
	}
	aliveMap, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user alive error: %w", err)
	}
	return &panelSnapshot{
		Node:  node,
		Users: cloneUserList(users),
		Alive: cloneAliveList(aliveMap),
	}, nil
}

func (c *Controller) saveCurrentPanelSnapshot() {
	if c.info == nil {
		return
	}
	if err := c.savePanelSnapshot(&panelSnapshot{
		Node:  c.info,
		Users: c.userList,
		Alive: c.aliveList,
	}); err != nil {
		log.WithError(err).Warn("save panel cache failed")
	}
}

func (c *Controller) savePanelSnapshot(snapshot *panelSnapshot) error {
	if snapshot == nil || snapshot.Node == nil {
		return fmt.Errorf("empty panel snapshot")
	}
	if err := snapshot.Node.Normalize(c.conf.NodeID, c.apiClient.APIHost); err != nil {
		return err
	}
	toSave := panelSnapshot{
		APIHost: c.apiClient.APIHost,
		NodeID:  c.conf.NodeID,
		Node:    snapshot.Node,
		Users:   cloneUserList(snapshot.Users),
		Alive:   cloneAliveList(snapshot.Alive),
		SavedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(toSave)
	if err != nil {
		return err
	}
	cachePath := c.panelSnapshotPath()
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return err
	}
	tmpPath := cachePath + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, cachePath)
}

func (c *Controller) readPanelSnapshot() (*panelSnapshot, error) {
	body, err := os.ReadFile(c.panelSnapshotPath())
	if err != nil {
		return nil, err
	}
	var snapshot panelSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Node == nil {
		return nil, fmt.Errorf("cached node info is empty")
	}
	if snapshot.APIHost != "" && snapshot.APIHost != c.apiClient.APIHost {
		return nil, fmt.Errorf("cached api host %q does not match %q", snapshot.APIHost, c.apiClient.APIHost)
	}
	if snapshot.NodeID != 0 && snapshot.NodeID != c.conf.NodeID {
		return nil, fmt.Errorf("cached node id %d does not match %d", snapshot.NodeID, c.conf.NodeID)
	}
	if err := snapshot.Node.Normalize(c.conf.NodeID, c.apiClient.APIHost); err != nil {
		return nil, err
	}
	snapshot.Users = cloneUserList(snapshot.Users)
	snapshot.Alive = cloneAliveList(snapshot.Alive)
	return &snapshot, nil
}

func (c *Controller) panelSnapshotPath() string {
	return filepath.Join(c.runtime.WorkingDir, fmt.Sprintf("node-%d", c.conf.NodeID), "panel-cache.json")
}

func (c *Controller) triggerReload() {
	select {
	case c.reloadCh <- struct{}{}:
	default:
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) error {
	newInfo, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithError(err).Error("get node info failed")
		return nil
	}
	if newInfo != nil {
		if err := c.savePanelSnapshot(&panelSnapshot{
			Node:  newInfo,
			Users: c.userList,
			Alive: c.aliveList,
		}); err != nil {
			log.WithError(err).Warn("save panel cache failed")
		}
		c.triggerReload()
		return nil
	}

	newUsers, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithError(err).Error("get user list failed")
		return nil
	}
	newAlive, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithError(err).Error("get alive list failed")
		return nil
	}
	if newAlive != nil {
		c.aliveList = cloneAliveList(newAlive)
		c.server.SetAliveList(newAlive)
	}
	if newUsers == nil {
		c.saveCurrentPanelSnapshot()
		return nil
	}
	deleted, added, modified := compareUserList(c.userList, newUsers)
	if len(added) > 0 || len(deleted) > 0 || len(modified) > 0 {
		if err := c.server.UpdateUsers(added, deleted, modified, newUsers); err != nil {
			log.WithError(err).Error("update sudoku users failed")
			c.triggerReload()
			return nil
		}
		c.userList = cloneUserList(newUsers)
		log.Infof("%s: %d users added, %d deleted, %d modified", c.tag, len(added), len(deleted), len(modified))
	}
	c.saveCurrentPanelSnapshot()
	return nil
}

func (c *Controller) reportUserTrafficTask(ctx context.Context) error {
	reportMin := 0
	deviceMin := 0
	if c.info != nil {
		reportMin = c.info.BaseConfig.NodeReportMinTraffic
		deviceMin = c.info.BaseConfig.DeviceOnlineMinTraffic
	}
	userTraffic := c.server.GetUserTrafficSlice(reportMin)
	if len(userTraffic) > 0 {
		if err := c.apiClient.ReportUserTraffic(ctx, userTraffic); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			log.WithError(err).Info("report user traffic failed")
		} else {
			c.server.ConfirmUserTraffic(userTraffic)
			log.Debugf("%s: reported %d users traffic", c.tag, len(userTraffic))
		}
	}

	onlineDevice := c.server.GetOnlineDevice()
	if len(onlineDevice) == 0 {
		return nil
	}
	noCountUID := map[int]struct{}{}
	for _, traffic := range userTraffic {
		if traffic.Upload+traffic.Download < int64(deviceMin*1000) {
			noCountUID[traffic.UID] = struct{}{}
		}
	}
	reportData := map[int][]string{}
	for _, online := range onlineDevice {
		if _, skip := noCountUID[online.UID]; skip {
			continue
		}
		reportData[online.UID] = append(reportData[online.UID], online.IP)
	}
	if len(reportData) > 0 {
		if err := c.apiClient.ReportNodeOnlineUsers(ctx, reportData); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			log.WithError(err).Info("report online users failed")
		}
	}
	return nil
}

func compareUserList(oldUsers, newUsers []panel.UserInfo) (deleted, added, modified []panel.UserInfo) {
	oldMap := make(map[string]panel.UserInfo, len(oldUsers))
	for _, user := range oldUsers {
		oldMap[user.Uuid] = user
	}
	for _, user := range newUsers {
		if existing, ok := oldMap[user.Uuid]; !ok {
			added = append(added, user)
		} else {
			if existing.SpeedLimit != user.SpeedLimit || existing.DeviceLimit != user.DeviceLimit || existing.Email != user.Email {
				modified = append(modified, user)
			}
			delete(oldMap, user.Uuid)
		}
	}
	for _, user := range oldMap {
		deleted = append(deleted, user)
	}
	return deleted, added, modified
}

func cloneUserList(in []panel.UserInfo) []panel.UserInfo {
	if len(in) == 0 {
		return []panel.UserInfo{}
	}
	out := make([]panel.UserInfo, len(in))
	copy(out, in)
	return out
}

func cloneAliveList(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	for uid, count := range in {
		out[uid] = count
	}
	return out
}

func SortedUserHashes(server runtime.Server) map[string]int {
	embedded, ok := server.(interface{ DebugUserHashMap() map[string]int })
	if !ok {
		return nil
	}
	raw := embedded.DebugUserHashMap()
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := map[string]int{}
	for _, key := range keys {
		out[key] = raw[key]
	}
	return out
}
