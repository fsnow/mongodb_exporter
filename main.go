// mongodb_exporter
// Copyright (C) 2017 Percona LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/sirupsen/logrus"

	"github.com/percona/mongodb_exporter/exporter"
)

//nolint:gochecknoglobals
var (
	version   string
	commit    string
	buildDate string
)

// GlobalFlags has command line flags to configure the exporter.
type GlobalFlags struct {
	URI              string `name:"mongodb.uri" help:"MongoDB connection URI" env:"MONGODB_URI" placeholder:"mongodb://user:pass@127.0.0.1:27017/admin?ssl=true"`
	GlobalConnPool   bool   `name:"mongodb.global-conn-pool" help:"Use global connection pool instead of creating new pool for each http request." negatable:""`
	DirectConnect    bool   `name:"mongodb.direct-connect" help:"Whether or not a direct connect should be made. Direct connections are not valid if multiple hosts are specified or an SRV URI is used." default:"true" negatable:""`
	WebListenAddress string `name:"web.listen-address" help:"Address to listen on for web interface and telemetry" default:":9216"`
	WebTelemetryPath string `name:"web.telemetry-path" help:"Metrics expose path" default:"/metrics"`
	TLSConfigPath    string `name:"web.config" help:"Path to the file having Prometheus TLS config for basic auth"`
	LogLevel         string `name:"log.level" help:"Only log messages with the given severity or above. Valid levels: [debug, info, warn, error, fatal]" enum:"debug,info,warn,error,fatal" default:"error"`
	Version          bool   `name:"version" help:"Show version and exit"`
}

func main() {
	var opts GlobalFlags
	_ = kong.Parse(&opts,
		kong.Name("mongodb_exporter"),
		kong.Description("MongoDB Prometheus exporter"),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.Vars{
			"version": version,
		})

	if opts.Version {
		fmt.Println("mongodb_exporter - MongoDB Prometheus exporter")
		fmt.Printf("Version: %s\n", version)
		fmt.Printf("Commit: %s\n", commit)
		fmt.Printf("Build date: %s\n", buildDate)

		return
	}

	e := buildExporter(opts)
	e.Run()
}

func buildExporter(opts GlobalFlags) *exporter.Exporter {
	log := logrus.New()

	levels := map[string]logrus.Level{
		"debug": logrus.DebugLevel,
		"error": logrus.ErrorLevel,
		"fatal": logrus.FatalLevel,
		"info":  logrus.InfoLevel,
		"warn":  logrus.WarnLevel,
	}
	log.SetLevel(levels[opts.LogLevel])

	log.Debugf("Compatible mode: %v", true)

	if !strings.HasPrefix(opts.URI, "mongodb") {
		log.Debugf("Prepending mongodb:// to the URI")
		opts.URI = "mongodb://" + opts.URI
	}

	log.Debugf("Connection URI: %s", opts.URI)

	exporterOpts := &exporter.Opts{
		Logger:           log,
		Path:             opts.WebTelemetryPath,
		URI:              opts.URI,
		GlobalConnPool:   opts.GlobalConnPool,
		WebListenAddress: opts.WebListenAddress,
		TLSConfigPath:    opts.TLSConfigPath,
		DirectConnect:    opts.DirectConnect,

		// all values below here previously were settable as command-line parameters but have been hard-coded in this special build
		CollStatsNamespaces:   make([]string, 0),
		CompatibleMode:        true,
		DiscoveringMode:       false,
		IndexStatsCollections: make([]string, 0),

		EnableDiagnosticData:   true,
		EnableReplicasetStatus: false,
		EnableTopMetrics:       false,
		EnableDBStats:          true,
		EnableIndexStats:       false,
		EnableCollStats:        false,

		EnableOverrideDescendingIndex: false,

		CollStatsLimit: 0,
		CollectAll:     false,
	}

	e := exporter.New(exporterOpts)

	return e
}
