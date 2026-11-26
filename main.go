/*
 * Copyright (c) 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SENERGY-Platform/mgw-dc-lib-go/pkg/configuration"
	"github.com/SENERGY-Platform/mgw-dc-lib-go/pkg/mgw"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/config"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/connector"
	"github.com/SENERGY-Platform/mgw-samsung-dc/pkg/samsung/ocf"
)

func main() {
	ec := 0
	defer func() { os.Exit(ec) }()

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		ec = 1
		return
	}

	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if cfg.DeviceTypeId == "" {
		logger.Warn("DEVICE_TYPE_ID is not set; the device is announced without a SENERGY device type")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	conn := connector.New(cfg.DeviceTypeId, logger)

	// Sequentially, and before anything else touches the network: the first
	// ocf.New writes the client key and certificate if they are not there yet,
	// and every later one reads them. All appliances present the same client
	// identity, which is the identity their access control list grants.
	clients := make([]*ocf.Client, 0, len(cfg.Appliances))
	// Registered before the loop fills the slice, so a client built before one
	// of the later ones fails is closed too.
	defer func() {
		for _, ocfClient := range clients {
			if cerr := ocfClient.Close(); cerr != nil {
				logger.Warn("error closing ocf client", "error", cerr)
			}
		}
	}()
	for _, appliance := range cfg.Appliances {
		ocfClient, err := ocf.New(appliance.Host, appliance.Port, cfg.CertPath, cfg.OcfClientUUID)
		if err != nil {
			logger.Error("unable to open ocf client", "host", appliance.Addr(), "error", err)
			ec = 1
			return
		}
		clients = append(clients, ocfClient)
		conn.Add(ocfClient, appliance.Addr())
	}

	wg := &sync.WaitGroup{}
	mgwClient, err := mgw.New[connector.Device](configuration.Config{
		ConnectorId:   cfg.ConnectorId,
		MgwMqttBroker: cfg.MgwMqttBroker,
		MgwMqttUser:   cfg.MgwMqttUser,
		MgwMqttPw:     cfg.MgwMqttPw,
		Debug:         cfg.Debug,
	}, ctx, wg, conn.Refresh)
	if err != nil {
		logger.Error("unable to connect to mgw broker", "error", err)
		ec = 1
		return
	}

	conn.Register(mgwClient, cfg.PollIntervals)

	// Start returns at once. Each appliance announces itself as soon as it
	// answers; an unreachable one keeps retrying without holding back the
	// others, so the connector runs from here on whatever the appliances do.
	conn.Start(ctx, mgwClient)

	logger.Info("connector running", "connector_id", cfg.ConnectorId,
		"appliances", appliancesForLog(cfg.Appliances),
		"poll_intervals", pollIntervalsForLog(cfg.PollIntervals))
	<-ctx.Done()
	logger.Info("shutting down")
	wg.Wait()
}

// appliancesForLog renders the configured appliance addresses for the startup
// log line.
func appliancesForLog(appliances []config.Appliance) string {
	addrs := make([]string, 0, len(appliances))
	for _, a := range appliances {
		addrs = append(addrs, a.Addr())
	}
	return strings.Join(addrs, " ")
}

// pollIntervalsForLog renders the configured poll intervals for the startup log
// line, sorted by service so two starts of the same configuration log the same
// string.
func pollIntervalsForLog(intervals map[string]time.Duration) string {
	services := make([]string, 0, len(intervals))
	for svc := range intervals {
		services = append(services, svc)
	}
	sort.Strings(services)
	parts := make([]string, 0, len(services))
	for _, svc := range services {
		parts = append(parts, svc+"="+intervals[svc].String())
	}
	return strings.Join(parts, " ")
}
