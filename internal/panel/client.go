package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"github.com/vmihailenco/msgpack/v5"
)

type Client struct {
	client           *resty.Client
	APIHost          string
	Token            string
	NodeID           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
}

type NodeInfo struct {
	Id                 int
	Tag                string
	Protocol           string          `json:"protocol"`
	Host               string          `json:"host"`
	ListenIP           string          `json:"listen_ip"`
	Port               any             `json:"port"`
	ServerPort         int             `json:"server_port"`
	Network            string          `json:"network"`
	NetworkSettings    json.RawMessage `json:"network_settings"`
	Encryption         string          `json:"encryption"`
	EncryptionSettings SudokuSettings  `json:"encryption_settings"`
	BaseConfig         BaseConfig      `json:"base_config"`
	PushInterval       time.Duration
	PullInterval       time.Duration
}

type BaseConfig struct {
	PushInterval           any `json:"push_interval"`
	PullInterval           any `json:"pull_interval"`
	DeviceOnlineMinTraffic int `json:"device_online_min_traffic"`
	NodeReportMinTraffic   int `json:"node_report_min_traffic"`
}

type SudokuSettings struct {
	MasterPublicKey    string   `json:"master_public_key"`
	MasterPrivateKey   string   `json:"master_private_key"`
	AEAD               string   `json:"aead"`
	ASCII              string   `json:"ascii"`
	PaddingMin         int      `json:"padding_min"`
	PaddingMax         int      `json:"padding_max"`
	EnablePureDownlink *bool    `json:"enable_pure_downlink"`
	Multiplex          string   `json:"multiplex"`
	FallbackAddress    string   `json:"fallback_address"`
	SuspiciousAction   string   `json:"suspicious_action"`
	CustomTable        string   `json:"custom_table"`
	CustomTables       []string `json:"custom_tables"`
	HTTPMask           struct {
		Disable   *bool  `json:"disable"`
		Mode      string `json:"mode"`
		TLS       *bool  `json:"tls"`
		Host      string `json:"host"`
		PathRoot  string `json:"path_root"`
		Multiplex string `json:"multiplex"`
	} `json:"httpmask"`
}

type UserInfo struct {
	Id          int    `json:"id" msgpack:"id"`
	Uuid        string `json:"uuid" msgpack:"uuid"`
	Email       string `json:"email" msgpack:"email"`
	SpeedLimit  int    `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit int    `json:"device_limit" msgpack:"device_limit"`
}

type UserListBody struct {
	Users []UserInfo `json:"users" msgpack:"users"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

type UserTraffic struct {
	UID      int
	Upload   int64
	Download int64
	UUID     string `json:"-"`
}

type OnlineUser struct {
	UID int
	IP  string
}

func New(c *conf.NodeConfig) (*Client, error) {
	client := resty.New()
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("v2sudoku go-resty/%s", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var respErr *resty.ResponseError
		if errors.As(err, &respErr) {
			log.Error(respErr.Err)
		}
	})
	client.SetBaseURL(c.APIHost)
	client.SetQueryParams(map[string]string{
		"node_type": "v2node",
		"node_id":   strconv.Itoa(c.NodeID),
		"token":     c.Key,
	})
	return &Client{
		client:  client,
		Token:   c.Key,
		APIHost: c.APIHost,
		NodeID:  c.NodeID,
	}, nil
}

func (c *Client) GetNodeInfo(ctx context.Context) (*NodeInfo, error) {
	const path = "/api/v2/server/config"
	resp, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.nodeEtag).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("received nil response")
	}
	if resp.StatusCode() == 304 {
		return nil, nil
	}
	if err := checkResponseStatus(resp); err != nil {
		return nil, err
	}

	hash := sha256.Sum256(resp.Body())
	newBodyHash := hex.EncodeToString(hash[:])
	if c.responseBodyHash == newBodyHash {
		return nil, nil
	}
	c.responseBodyHash = newBodyHash
	c.nodeEtag = strings.Trim(resp.Header().Get("ETag"), "\"")

	var cfg NodeInfo
	if err := json.Unmarshal(resp.Body(), &cfg); err != nil {
		return nil, fmt.Errorf("decode node params error: %w", err)
	}
	if err := cfg.Normalize(c.NodeID, c.APIHost); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (cfg *NodeInfo) Normalize(nodeID int, apiHost string) error {
	if cfg.Protocol != "sudoku" {
		return fmt.Errorf("unsupported protocol: %s", cfg.Protocol)
	}
	cfg.Id = nodeID
	cfg.Tag = fmt.Sprintf("[%s]-sudoku:%d", apiHost, nodeID)
	if cfg.ListenIP == "" {
		cfg.ListenIP = "0.0.0.0"
	}
	if cfg.ServerPort <= 0 {
		return fmt.Errorf("server_port is required")
	}
	if cfg.BaseConfig.PushInterval != nil {
		cfg.PushInterval = intervalToDuration(cfg.BaseConfig.PushInterval)
	}
	if cfg.PushInterval <= 0 {
		cfg.PushInterval = time.Minute
	}
	if cfg.BaseConfig.PullInterval != nil {
		cfg.PullInterval = intervalToDuration(cfg.BaseConfig.PullInterval)
	}
	if cfg.PullInterval <= 0 {
		cfg.PullInterval = time.Minute
	}
	if cfg.EncryptionSettings.MasterPublicKey == "" && cfg.EncryptionSettings.MasterPrivateKey == "" {
		return fmt.Errorf("encryption_settings.master_public_key or master_private_key is required")
	}
	return nil
}

func intervalToDuration(value any) time.Duration {
	if value == nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return time.Duration(v) * time.Second
	case int64:
		return time.Duration(v) * time.Second
	case float64:
		return time.Duration(v) * time.Second
	case string:
		i, _ := strconv.Atoi(v)
		return time.Duration(i) * time.Second
	default:
		rv := reflect.ValueOf(value)
		if rv.Kind() >= reflect.Int && rv.Kind() <= reflect.Int64 {
			return time.Duration(rv.Int()) * time.Second
		}
		return 0
	}
}

func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	const path = "/api/v1/server/UniProxy/user"
	resp, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(path)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer resp.RawResponse.Body.Close()

	if resp.StatusCode() == 304 {
		return nil, nil
	}
	if err := checkResponseStatus(resp); err != nil {
		return nil, err
	}

	userList := &UserListBody{}
	if strings.Contains(resp.Header().Get("Content-Type"), "application/x-msgpack") {
		if err := msgpack.NewDecoder(resp.RawResponse.Body).Decode(userList); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	} else if err := json.NewDecoder(resp.RawResponse.Body).Decode(userList); err != nil {
		return nil, fmt.Errorf("decode user list error: %w", err)
	}
	c.userEtag = resp.Header().Get("ETag")
	return userList.Users, nil
}

func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	const path = "/api/v1/server/UniProxy/alivelist"
	resp, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return map[int]int{}, nil
	}
	if resp == nil || resp.RawResponse == nil || resp.StatusCode() >= 399 {
		return map[int]int{}, nil
	}
	defer resp.RawResponse.Body.Close()

	alive := &AliveMap{}
	if err := json.Unmarshal(resp.Body(), alive); err != nil {
		return map[int]int{}, nil
	}
	if alive.Alive == nil {
		return map[int]int{}, nil
	}
	return alive.Alive, nil
}

func (c *Client) ReportUserTraffic(ctx context.Context, userTraffic []UserTraffic) error {
	data := make(map[int][]int64, len(userTraffic))
	for _, traffic := range userTraffic {
		current := data[traffic.UID]
		if len(current) == 0 {
			current = []int64{0, 0}
		}
		current[0] += traffic.Upload
		current[1] += traffic.Download
		data[traffic.UID] = current
	}
	const path = "/api/v1/server/UniProxy/push"
	resp, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	return checkResponseStatus(resp)
}

func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data map[int][]string) error {
	const path = "/api/v1/server/UniProxy/alive"
	resp, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	return checkResponseStatus(resp)
}

func checkResponseStatus(resp *resty.Response) error {
	if resp == nil {
		return fmt.Errorf("received nil response")
	}
	if resp.StatusCode() < 400 {
		return nil
	}
	return &StatusError{
		StatusCode: resp.StatusCode(),
		Body:       shortResponseBody(resp.Body()),
	}
}

type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("panel request failed: status=%d body=%s", e.StatusCode, e.Body)
}

func IsAuthStatusError(err error) bool {
	var statusErr *StatusError
	return errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden)
}

func shortResponseBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 256 {
		return text[:256]
	}
	return text
}
