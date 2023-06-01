// mongodb_exporter
// Copyright (C) 2022 Percona LLC
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
	"testing"
)

func TestBuildExporter(t *testing.T) {
	opts := GlobalFlags{
		URI:              "mongodb://usr:pwd@127.0.0.1/",
		GlobalConnPool:   false, // to avoid testing the connection
		WebListenAddress: "localhost:12345",
		WebTelemetryPath: "/mymetrics",
		LogLevel:         "debug",
	}

	buildExporter(opts)
}
