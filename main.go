package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/conf"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/controller"
	panel "github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/panel"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/runtime"
	"github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/version"
	log "github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("config", "config.yml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("v2sudoku %s commit %s\n", version.Version, version.Commit)
		return
	}

	cfg := conf.New()
	if err := cfg.LoadFromPath(*configPath); err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	if err := setupLog(cfg.LogConfig); err != nil {
		log.Fatalf("setup log failed: %v", err)
	}
	log.WithFields(log.Fields{
		"version": version.Version,
		"commit":  version.Commit,
		"engine":  cfg.RuntimeConfig.EngineName(),
	}).Info("v2sudoku starting")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if len(cfg.NodeConfigs) == 0 {
		log.Fatal("no node configured")
	}
	keyStore := runtime.NewClientKeyStore(cfg.RuntimeConfig)
	for i := range cfg.NodeConfigs {
		nodeCfg := cfg.NodeConfigs[i]
		go runNode(ctx, &nodeCfg, cfg.RuntimeConfig, keyStore)
	}

	<-ctx.Done()
	log.Info("v2sudoku stopped")
}

func runNode(ctx context.Context, nodeCfg *conf.NodeConfig, runtimeConfig conf.RuntimeConfig, keyStore *runtime.ClientKeyStore) {
	reloadCh := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		client, err := panel.New(nodeCfg)
		if err != nil {
			log.WithError(err).Error("create panel client failed")
			sleepOrDone(ctx, 10*time.Second)
			continue
		}

		c := controller.New(client, nodeCfg, runtimeConfig, keyStore, reloadCh)
		if err := c.Start(); err != nil {
			log.WithFields(log.Fields{
				"node_id": nodeCfg.NodeID,
				"err":     err,
			}).Error("start node controller failed")
			sleepOrDone(ctx, 10*time.Second)
			continue
		}

		select {
		case <-ctx.Done():
			c.Close()
			return
		case <-reloadCh:
			c.Close()
			sleepOrDone(ctx, time.Second)
		}
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func setupLog(cfg conf.LogConfig) error {
	level, err := log.ParseLevel(cfg.Level)
	if err != nil {
		return err
	}
	log.SetLevel(level)
	log.SetFormatter(compactFormatter{})
	if cfg.Output != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Output), 0755); err != nil {
			return err
		}
		file, err := os.OpenFile(cfg.Output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		log.SetOutput(file)
	}
	return nil
}

type compactFormatter struct{}

func (compactFormatter) Format(entry *log.Entry) ([]byte, error) {
	var builder strings.Builder
	builder.WriteString(entry.Time.Format("2006/01/02 15:04:05"))
	builder.WriteByte(' ')
	builder.WriteByte('[')
	builder.WriteString(strings.ToUpper(entry.Level.String()))
	builder.WriteString("] ")
	builder.WriteString(entry.Message)
	if len(entry.Data) > 0 {
		keys := make([]string, 0, len(entry.Data))
		for key := range entry.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			builder.WriteByte(' ')
			builder.WriteString(key)
			builder.WriteByte('=')
			builder.WriteString(compactLogValue(entry.Data[key]))
		}
	}
	builder.WriteByte('\n')
	return []byte(builder.String()), nil
}

func compactLogValue(value any) string {
	text := fmt.Sprint(value)
	if text == "" {
		return strconv.Quote(text)
	}
	if strings.ContainsAny(text, " \t\r\n\"") {
		return strconv.Quote(text)
	}
	return text
}
