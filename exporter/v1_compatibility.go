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

package exporter

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/percona/percona-toolkit/src/go/mongolib/proto"
	"github.com/percona/percona-toolkit/src/go/mongolib/util"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// UnknownState is the values for an unknown rs state.
	// From MongoDB documentation: https://docs.mongodb.com/manual/reference/replica-states/
	UnknownState = 6

	// EnterpriseEdition shows that MongoDB is Enterprise edition.
	EnterpriseEdition = "Enterprise"
	// CommunityEdition shows that MongoDB is Community edition.
	CommunityEdition = "Community"

	// PerconaVendor means that MongoDB provided by Percona.
	PerconaVendor = "Percona"
	// MongoDBVendor means that MongoDB provided by Mongo.
	MongoDBVendor = "MongoDB"
)

// ErrInvalidMetricValue cannot create a new metric due to an invalid value.
var errInvalidMetricValue = fmt.Errorf("invalid metric value")

/*
  This is used to convert a new metric like: mongodb_ss_asserts{assert_type=*} (1)
  to the old-compatible metric:  mongodb_mongod_asserts_total{type="regular|warning|msg|user|rollovers"}.
  In this particular case, conversion would be:
  conversion {
   newName: "mongodb_ss_asserts",
   oldName: "mongodb_mongod_asserts_total",
   labels : map[string]string{ "assert_type": "type"},
  }.

  Some other metric renaming are more complex. (2)
  In some cases, there is a total renaming, with new labels and the only part we can use to identify a metric
  is its prefix. Example:
  Metrics like mongodb_ss_metrics_operation _fastmod or
  mongodb_ss_metrics_operation_idhack or
  mongodb_ss_metrics_operation_scanAndOrder
  should use the trim the "prefix" mongodb_ss_metrics_operation from the metric name, and that remaining suffic
  is the label value for a new label "suffixLabel".
  It means that the metric (current) mongodb_ss_metrics_operation_idhack will become into the old equivalent one
  mongodb_mongod_metrics_operation_total {"state": "idhack"} as defined in the conversion slice:
   {
     oldName:     "mongodb_mongod_metrics_operation_total", //{state="fastmod|idhack|scan_and_order"}
     prefix:      "mongodb_ss_metrics_operation",           // _[fastmod|idhack|scanAndOrder]
     suffixLabel: "state",
   },

   suffixMapping field:
   --------------------
   Also, some metrics suffixes for the second renaming case need a mapping between the old and new values.
   For example, the metric mongodb_ss_wt_cache_bytes_currently_in_the_cache has mongodb_ss_wt_cache_bytes
   as the prefix so the suffix is bytes_currently_in_the_cache should be converted to a mertic named
   mongodb_mongod_wiredtiger_cache_bytes and the suffix bytes_currently_in_the_cache is being mapped to
   "total".

   Third renaming form: see (3) below.
*/

// For simple metric renaming, only some fields should be updated like the metric name, the help and some
// labels that have 1 to 1 mapping (1).
func newToOldMetric(rm *rawMetric, c conversion) *rawMetric {
	oldMetric := &rawMetric{
		fqName: c.oldName,
		help:   rm.help,
		val:    rm.val,
		vt:     rm.vt,
		ln:     make([]string, 0, len(rm.ln)),
		lv:     make([]string, 0, len(rm.lv)),
	}

	for _, val := range rm.lv {
		if newLabelVal, ok := c.labelValueConversions[val]; ok {
			oldMetric.lv = append(oldMetric.lv, newLabelVal)
			continue
		}
		oldMetric.lv = append(oldMetric.lv, val)
	}

	// Some label names should be converted from the new (current) name to the
	// mongodb_exporter v1 compatible name
	for _, newLabelName := range rm.ln {
		// if it should be converted, append the old-compatible name
		if oldLabel, ok := c.labelConversions[newLabelName]; ok {
			oldMetric.ln = append(oldMetric.ln, oldLabel)
			continue
		}
		// otherwise, keep the same label name
		oldMetric.ln = append(oldMetric.ln, newLabelName)
	}

	//fmt.Printf("%+v\n", oldMetric)
	return oldMetric
}

// The second renaming case is not a direct rename. In this case, the new metric name has a common
// prefix and the rest of the metric name is used as the value for a label in tne old metric style. (2)
// In this renaming case, the metric "mongodb_ss_wt_cache_bytes_bytes_currently_in_the_cache
// should be converted to mongodb_mongod_wiredtiger_cache_bytes with label "type": "total".
// For this conversion, we have the suffixMapping field that holds the mapping for all suffixes.
// Example definition:
//
//	 oldName:     "mongodb_mongod_wiredtiger_cache_bytes",
//	 prefix:      "mongodb_ss_wt_cache_bytes",
//	 suffixLabel: "type",
//	 suffixMapping: map[string]string{
//	   "bytes_currently_in_the_cache":                           "total",
//	   "tracked_dirty_bytes_in_the_cache":                       "dirty",
//	   "tracked_bytes_belonging_to_internal_pages_in_the_cache": "internal_pages",
//	   "tracked_bytes_belonging_to_leaf_pages_in_the_cache":     "internal_pages",
//	 },
//	},
func createOldMetricFromNew(rm *rawMetric, c conversion) *rawMetric {
	suffix := strings.TrimPrefix(rm.fqName, c.prefix)
	suffix = strings.TrimPrefix(suffix, "_")

	if newSuffix, ok := c.suffixMapping[suffix]; ok {
		suffix = newSuffix
	}

	oldMetric := &rawMetric{
		fqName: c.oldName,
		help:   c.oldName,
		val:    rm.val,
		vt:     rm.vt,
		ln:     []string{c.suffixLabel},
		lv:     []string{suffix},
	}

	return oldMetric
}

func cacheEvictedTotalMetric(m bson.M) (prometheus.Metric, error) {
	s, err := sumMetrics(m, [][]string{
		{"serverStatus", "wiredTiger", "cache", "modified pages evicted"},
		{"serverStatus", "wiredTiger", "cache", "unmodified pages evicted"},
	})
	if err != nil {
		return nil, err
	}

	d := prometheus.NewDesc("mongodb_mongod_wiredtiger_cache_evicted_total", "wiredtiger cache evicted total", nil, nil)
	metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, s)
	if err != nil {
		return nil, err
	}

	return metric, nil
}

func sumMetrics(m bson.M, paths [][]string) (float64, error) {
	var total float64

	for _, path := range paths {
		v := walkTo(m, path)
		if v == nil {
			continue
		}

		f, err := asFloat64(v)
		if err != nil {
			return 0, errors.Wrapf(errInvalidMetricValue, "%v", v)
		}

		total += *f
	}

	return total, nil
}

// Converts new metric to the old metric style and append it to the response slice.
func appendCompatibleMetric(res []prometheus.Metric, rm *rawMetric) []prometheus.Metric {
	compatibleMetrics := metricRenameAndLabel(rm, conversions())
	if compatibleMetrics == nil {
		return res
	}

	for _, compatibleMetric := range compatibleMetrics {
		metric, err := rawToPrometheusMetric(compatibleMetric)
		if err != nil {
			invalidMetric := prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err)
			res = append(res, invalidMetric)
			return res
		}

		res = append(res, metric)
	}

	return res
}

//nolint:funlen
func conversions() []conversion {
	return []conversion{
		// FSnow: three renames that we need
		{
			oldName: "mongodb_mongod_replset_oplog_size_bytes",
			newName: "mongodb_oplog_stats_storageSize",
		},
		{
			oldName: "mongodb_mongod_db_data_size_bytes",
			newName: "mongodb_dbstats_dataSize",
		},
		{
			oldName: "mongodb_mongod_db_indexes_total",
			newName: "mongodb_dbstats_indexes",
		},
	}
}

// Third metric renaming case (3).
// Lock* metrics don't fit in (1) nor in (2) and since they are just a few, and we know they always exists
// as part of getDiagnosticData, we can just call locksMetrics with getDiagnosticData result as the input
// to get the v1 compatible metrics from the new structure.

type lockMetric struct {
	name   string
	path   []string
	labels map[string]string
}

func lockMetrics() []lockMetric {
	return []lockMetric{
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ParallelBatchWriterMode", "acquireCount", "r"},
			labels: map[string]string{"lock_mode": "r", "resource": "ParallelBatchWriterMode"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ParallelBatchWriterMode", "acquireCount", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "acquireCount", "w"},
			labels: map[string]string{"resource": "ReplicationStateTransition", "lock_mode": "w"},
		},
		{
			name:   "mongodb_ss_locks_acquireWaitCount",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "acquireCount", "W"},
			labels: map[string]string{"lock_mode": "W", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_timeAcquiringMicros",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "timeAcquiringMicros", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "r"},
			labels: map[string]string{"lock_mode": "r", "resource": "Global"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "Global"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "W"},
			labels: map[string]string{"lock_mode": "W", "resource": "Global"},
		},
	}
}

// locksMetrics returns the list of lock metrics as a prometheus.Metric slice
// This function reads the human readable list from lockMetrics() and creates a slice of metrics
// ready to be exposed, taking the value for each metric from th provided bson.M structure from
// getDiagnosticData.
func locksMetrics(logger *logrus.Logger, m bson.M) []prometheus.Metric {
	metrics := lockMetrics()
	res := make([]prometheus.Metric, 0, len(metrics))

	for _, lm := range metrics {
		mm, err := makeLockMetric(m, lm)
		if mm == nil {
			continue
		}
		if err != nil {
			logger.Errorf("cannot convert lock metric %s to old style: %s", mm.Desc(), err)
			continue
		}
		res = append(res, mm)
	}

	return res
}

func makeLockMetric(m bson.M, lm lockMetric) (prometheus.Metric, error) {
	val := walkTo(m, lm.path)
	if val == nil {
		return nil, nil
	}

	f, err := asFloat64(val)
	if err != nil {
		return prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err), err
	}

	if f == nil {
		return nil, nil
	}

	ln := make([]string, 0, len(lm.labels))
	lv := make([]string, 0, len(lm.labels))
	for labelName, labelValue := range lm.labels {
		ln = append(ln, labelName)
		lv = append(lv, labelValue)
	}

	d := prometheus.NewDesc(lm.name, lm.name, ln, nil)

	return prometheus.NewConstMetric(d, prometheus.UntypedValue, *f, lv...)
}

type specialMetric struct {
	paths  [][]string
	labels map[string]string
	name   string
	help   string
}

func specialMetricDefinitions() []specialMetric {
	return []specialMetric{
		{
			name: "mongodb_mongod_locks_time_acquiring_global_microseconds_total",
			help: "sum of serverStatus.locks.Global.timeAcquiringMicros.[r|w]",
			paths: [][]string{
				{"serverStatus", "locks", "Global", "timeAcquiringMicros", "r"},
				{"serverStatus", "locks", "Global", "timeAcquiringMicros", "w"},
			},
		},
	}
}

func specialMetrics(ctx context.Context, client *mongo.Client, m bson.M, l *logrus.Logger) []prometheus.Metric {
	metrics := make([]prometheus.Metric, 0)

	if opLogMetrics, err := oplogStatus(ctx, client); err != nil {
		l.Warnf("cannot create metrics for oplog: %s", err)
	} else {
		metrics = append(metrics, opLogMetrics...)
	}

	return metrics
}

func retrieveMongoDBBuildInfo(ctx context.Context, client *mongo.Client, l *logrus.Logger) (buildInfo, error) {
	buildInfoCmd := bson.D{bson.E{Key: "buildInfo", Value: 1}}
	res := client.Database("admin").RunCommand(ctx, buildInfoCmd)

	var buildInfoDoc bson.M
	err := res.Decode(&buildInfoDoc)
	if err != nil {
		return buildInfo{}, errors.Wrap(err, "Failed to run buildInfo command")
	}

	modules, ok := buildInfoDoc["modules"].(bson.A)
	if !ok {
		return buildInfo{}, errors.Wrap(err, "Failed to cast module information variable")
	}

	var bi buildInfo
	if len(modules) > 0 && modules[0].(string) == "enterprise" {
		bi.Edition = EnterpriseEdition
	} else {
		bi.Edition = CommunityEdition
	}
	l.Debug("MongoDB edition: ", bi.Edition)

	_, ok = buildInfoDoc["psmdbVersion"]
	if ok {
		bi.Vendor = PerconaVendor
	} else {
		bi.Vendor = MongoDBVendor
	}

	bi.Version, ok = buildInfoDoc["version"].(string)
	if !ok {
		return buildInfo{}, errors.Wrap(err, "Failed to cast version information variable")
	}

	return bi, nil
}

func storageEngine(m bson.M) (prometheus.Metric, error) { //nolint:ireturn
	v := walkTo(m, []string{"serverStatus", "storageEngine", "name"})
	name := "mongodb_mongod_storage_engine"
	help := "The storage engine used by the MongoDB instance"

	engine, ok := v.(string)
	if !ok {
		return nil, errors.New("Engine is unavailable")
	}
	labels := map[string]string{"engine": engine}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(1))
	if err != nil {
		return nil, errors.Wrap(err, "cannot create metric for engine type")
	}
	return metric, nil
}

func serverVersion(bi buildInfo) prometheus.Metric { //nolint:ireturn
	name := "mongodb_version_info"
	help := "The server version"

	labels := map[string]string{"mongodb": bi.Version, "edition": bi.Edition, "vendor": bi.Vendor}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(1))

	return metric
}

func myState(ctx context.Context, client *mongo.Client) prometheus.Metric {
	state, err := util.MyState(ctx, client)
	if err != nil {
		state = UnknownState
	}

	var id string
	rs, err := util.ReplicasetConfig(ctx, client)
	if err == nil {
		id = rs.Config.ID
	}

	name := "mongodb_mongod_replset_my_state"
	help := "An integer between 0 and 10 that represents the replica state of the current member"

	labels := map[string]string{"set": id}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(state))

	return metric
}

func oplogStatus(ctx context.Context, client *mongo.Client) ([]prometheus.Metric, error) {
	oplogRS := client.Database("local").Collection("oplog.rs")
	type oplogRSResult struct {
		Timestamp primitive.Timestamp `bson:"ts"`
	}
	var head oplogRSResult
	headRes := oplogRS.FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.M{
		"$natural": -1,
	}))
	if headRes.Err() != nil {
		return nil, headRes.Err()
	}

	if err := headRes.Decode(&head); err != nil {
		return nil, err
	}

	headDesc := prometheus.NewDesc("mongodb_mongod_replset_oplog_head_timestamp",
		"The timestamp of the newest change in the oplog", nil, nil)
	headMetric := prometheus.MustNewConstMetric(headDesc, prometheus.GaugeValue, float64(head.Timestamp.T))

	return []prometheus.Metric{headMetric}, nil
}

func replSetMetrics(m bson.M) []prometheus.Metric {
	replSetGetStatus, ok := m["replSetGetStatus"].(bson.M)
	if !ok {
		return nil
	}
	var repl proto.ReplicaSetStatus
	b, err := bson.Marshal(replSetGetStatus)
	if err != nil {
		return nil
	}
	if err := bson.Unmarshal(b, &repl); err != nil {
		return nil
	}

	var primaryOpTime time.Time
	gotPrimary := false

	var metrics []prometheus.Metric
	// Find primary
	for _, m := range repl.Members {
		if m.StateStr == "PRIMARY" {
			primaryOpTime = m.OptimeDate.Time()
			gotPrimary = true

			break
		}
	}

	createMetric := func(name, help string, value float64, labels map[string]string) {
		const prefix = "mongodb_mongod_replset_"
		d := prometheus.NewDesc(prefix+name, help, nil, labels)
		metrics = append(metrics, prometheus.MustNewConstMetric(d, prometheus.GaugeValue, value))
	}

	createMetric("number_of_members",
		"The number of replica set members.",
		float64(len(repl.Members)), map[string]string{
			"set": repl.Set,
		})

	for _, m := range repl.Members {
		labels := map[string]string{
			"name":  m.Name,
			"state": m.StateStr,
			"set":   repl.Set,
		}
		if m.Self {
			createMetric("my_name", "The replica state name of the current member.", 1, map[string]string{
				"name": m.Name,
				"set":  repl.Set,
			})
		}

		if !m.ElectionTime.IsZero() {
			createMetric("member_election_date",
				"The timestamp the node was elected as replica leader",
				float64(m.ElectionTime.T), labels)
		}
		if t := m.OptimeDate.Time(); gotPrimary && !t.IsZero() && m.StateStr != "PRIMARY" {
			val := math.Abs(float64(t.Unix() - primaryOpTime.Unix()))
			createMetric("member_replication_lag",
				"The replication lag that this member has with the primary.",
				val, labels)
		}
		if m.PingMs != nil {
			createMetric("member_ping_ms",
				"The pingMs represents the number of milliseconds (ms) that a round-trip packet takes to travel between the remote member and the local instance.",
				*m.PingMs, labels)
		}
		if t := m.LastHeartbeat.Time(); !t.IsZero() {
			createMetric("member_last_heartbeat",
				"The lastHeartbeat value provides an ISODate formatted date and time of the transmission time of last heartbeat received from this member.",
				float64(t.Unix()), labels)
		}
		if t := m.LastHeartbeatRecv.Time(); !t.IsZero() {
			createMetric("member_last_heartbeat_recv",
				"The lastHeartbeatRecv value provides an ISODate formatted date and time that the last heartbeat was received from this member.",
				float64(t.Unix()), labels)
		}
		if m.ConfigVersion > 0 {
			createMetric("member_config_version",
				"The configVersion value is the replica set configuration version.",
				m.ConfigVersion, labels)
		}
	}
	return metrics
}

func mongosMetrics(ctx context.Context, client *mongo.Client, l *logrus.Logger) []prometheus.Metric {
	metrics := make([]prometheus.Metric, 0)

	if metric, err := databasesTotalPartitioned(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := databasesTotalUnpartitioned(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := shardedCollectionsTotal(ctx, client); err != nil {
		l.Debugf("cannot create metric for collections total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := balancerEnabled(ctx, client); err != nil {
		l.Debugf("cannot create metric for balancer is enabled: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	metric, err := chunksTotal(ctx, client)
	if err != nil {
		l.Debugf("cannot create metric for chunks total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	ms, err := chunksTotalPerShard(ctx, client)
	if err != nil {
		l.Debugf("cannot create metric for chunks total per shard: %s", err)
	} else {
		metrics = append(metrics, ms...)
	}

	if metric, err := chunksBalanced(ctx, client); err != nil {
		l.Debugf("cannot create metric for chunks balanced: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	ms, err = changelog10m(ctx, client, l)
	if err != nil {
		l.Errorf("cannot create metric for changelog: %s", err)
	} else {
		metrics = append(metrics, ms...)
	}

	metrics = append(metrics, dbstatsMetrics(ctx, client, l)...)

	if metric, err := shardingShardsTotal(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := shardingShardsDrainingTotal(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	return metrics
}

func databasesTotalPartitioned(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("databases").CountDocuments(ctx, bson.M{"partitioned": true})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_databases_total"
	help := "Total number of sharded databases"
	labels := map[string]string{"type": "partitioned"}

	d := prometheus.NewDesc(name, help, nil, labels)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func databasesTotalUnpartitioned(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("databases").CountDocuments(ctx, bson.M{"partitioned": false})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_databases_total"
	help := "Total number of sharded databases"
	labels := map[string]string{"type": "unpartitioned"}

	d := prometheus.NewDesc(name, help, nil, labels)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

// shardedCollectionsTotal gets total sharded collections.
func shardedCollectionsTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	collCount, err := client.Database("config").Collection("collections").CountDocuments(ctx, bson.M{"dropped": false})
	if err != nil {
		return nil, err
	}
	name := "mongodb_mongos_sharding_collections_total"
	help := "Total # of Collections with Sharding enabled"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(collCount))
}

func chunksBalanced(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	var m struct {
		InBalancerRound bool `bson:"inBalancerRound"`
	}

	cmd := bson.D{{Key: "balancerStatus", Value: "1"}}
	res := client.Database("admin").RunCommand(ctx, cmd)

	if err := res.Decode(&m); err != nil {
		return nil, err
	}

	value := float64(0)
	if !m.InBalancerRound {
		value = 1
	}

	name := "mongodb_mongos_sharding_chunks_is_balanced"
	help := "Shards are balanced"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, value)
}

func balancerEnabled(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	type bss struct {
		Stopped bool `bson:"stopped"`
	}
	var bs bss
	enabled := 0

	err := client.Database("config").Collection("settings").FindOne(ctx, bson.M{"_id": "balancer"}).Decode(&bs)
	if err != nil {
		return nil, err
	}

	if !bs.Stopped {
		enabled = 1
	}

	name := "mongodb_mongos_sharding_balancer_enabled"
	help := "Balancer is enabled"

	d := prometheus.NewDesc(name, help, nil, nil)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(enabled))

	return metric, nil
}

func chunksTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("chunks").CountDocuments(ctx, bson.M{})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_chunks_total"
	help := "Total number of chunks"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func chunksTotalPerShard(ctx context.Context, client *mongo.Client) ([]prometheus.Metric, error) {
	aggregation := bson.D{
		{Key: "$group", Value: bson.M{"_id": "$shard", "count": bson.M{"$sum": 1}}},
	}

	cursor, err := client.Database("config").Collection("chunks").Aggregate(ctx, mongo.Pipeline{aggregation})
	if err != nil {
		return nil, err
	}

	var shards []bson.M
	if err = cursor.All(ctx, &shards); err != nil {
		return nil, err
	}

	metrics := make([]prometheus.Metric, 0, len(shards))

	for _, shard := range shards {
		help := "Total number of chunks per shard"
		labels := map[string]string{"shard": shard["_id"].(string)}

		d := prometheus.NewDesc("mongodb_mongos_sharding_shard_chunks_total", help, nil, labels)
		val, ok := shard["count"].(int32)
		if !ok {
			continue
		}

		metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(val))
		if err != nil {
			continue
		}

		metrics = append(metrics, metric)
	}

	return metrics, nil
}

func shardingShardsTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_shards_total"
	help := "Total number of shards"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func shardingShardsDrainingTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{"draining": true})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_shards_draining_total"
	help := "Total number of drainingshards"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

// ShardingChangelogSummaryID Sharding Changelog Summary ID.
type ShardingChangelogSummaryID struct {
	Event string `bson:"event"`
	Note  string `bson:"note"`
}

// ShardingChangelogSummary Sharding Changelog Summary.
type ShardingChangelogSummary struct {
	ID    *ShardingChangelogSummaryID `bson:"_id"`
	Count float64                     `bson:"count"`
}

// ShardingChangelogStats is an array of Sharding changelog stats.
type ShardingChangelogStats struct {
	Items *[]ShardingChangelogSummary
}

func changelog10m(ctx context.Context, client *mongo.Client, l *logrus.Logger) ([]prometheus.Metric, error) {
	var metrics []prometheus.Metric

	coll := client.Database("config").Collection("changelog")
	match := bson.M{"time": bson.M{"$gt": time.Now().Add(-10 * time.Minute)}}
	group := bson.M{"_id": bson.M{"event": "$what", "note": "$details.note"}, "count": bson.M{"$sum": 1}}

	c, err := coll.Aggregate(ctx, []bson.M{{"$match": match}, {"$group": group}})
	if err != nil {
		return nil, errors.Wrap(err, "failed to aggregate sharding changelog events")
	}

	defer c.Close(ctx) //nolint:errcheck

	for c.Next(ctx) {
		s := &ShardingChangelogSummary{}
		if err := c.Decode(s); err != nil {
			l.Error(err)
			continue
		}

		name := "mongodb_mongos_sharding_changelog_10min_total"
		help := "mongodb_mongos_sharding_changelog_10min_total"

		labelValue := s.ID.Event
		if s.ID.Note != "" {
			labelValue += "." + s.ID.Note
		}

		d := prometheus.NewDesc(name, help, nil, map[string]string{"event": labelValue})
		metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, s.Count)
		if err != nil {
			continue
		}

		metrics = append(metrics, metric)
	}

	if err := c.Err(); err != nil {
		return nil, err
	}

	return metrics, nil
}

// DatabaseStatList contains stats from all databases.
type databaseStatList struct {
	Members []databaseStatus
}

// DatabaseStatus represents stats about a database (mongod and raw from mongos).
type databaseStatus struct {
	rawStatus                       // embed to collect top-level attributes
	Shards    map[string]*rawStatus `bson:"raw,omitempty"`
}

// RawStatus represents stats about a database from Mongos side.
type rawStatus struct {
	Name        string `bson:"db,omitempty"`
	IndexSize   int    `bson:"indexSize,omitempty"`
	DataSize    int    `bson:"dataSize,omitempty"`
	Collections int    `bson:"collections,omitempty"`
	Objects     int    `bson:"objects,omitempty"`
	Indexes     int    `bson:"indexes,omitempty"`
}

type buildInfo struct {
	Version string
	Edition string
	Vendor  string
}

func getDatabaseStatList(ctx context.Context, client *mongo.Client, l *logrus.Logger) *databaseStatList {
	dbStatList := &databaseStatList{}
	dbNames, err := client.ListDatabaseNames(ctx, bson.M{})
	if err != nil {
		l.Errorf("Failed to get database names: %s.", err)
		return nil
	}
	l.Debugf("getting stats for databases: %v", dbNames)
	for _, db := range dbNames {
		dbStatus := databaseStatus{}
		r := client.Database(db).RunCommand(context.TODO(), bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1}})
		err := r.Decode(&dbStatus)
		if err != nil {
			l.Errorf("Failed to get database status: %s.", err)
			return nil
		}
		dbStatList.Members = append(dbStatList.Members, dbStatus)
	}

	return dbStatList
}

func dbstatsMetrics(ctx context.Context, client *mongo.Client, l *logrus.Logger) []prometheus.Metric {
	var metrics []prometheus.Metric

	dbStatList := getDatabaseStatList(ctx, client, l)
	if dbStatList == nil {
		return metrics
	}

	for _, member := range dbStatList.Members {
		if len(member.Shards) > 0 {
			for shard, stats := range member.Shards {
				labels := prometheus.Labels{
					"db":    stats.Name,
					"shard": strings.Split(shard, "/")[0],
				}

				name := "mongodb_mongos_db_data_size_bytes"
				help := "The total size in bytes of the uncompressed data held in this database"

				d := prometheus.NewDesc(name, help, nil, labels)
				metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.DataSize))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_indexes_total"
				help = "Contains a count of the total number of indexes across all collections in the database"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.Indexes))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_index_size_bytes"
				help = "The total size in bytes of all indexes created on this database"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.IndexSize))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_collections_total"
				help = "Total number of collections"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.Collections))
				if err == nil {
					metrics = append(metrics, metric)
				}
			}
		}
	}

	return metrics
}

func walkTo(m primitive.M, path []string) interface{} {
	val, ok := m[path[0]]
	if !ok {
		return nil
	}

	if len(path) > 1 {
		switch v := val.(type) {
		case primitive.M:
			val = walkTo(v, path[1:])
		case map[string]interface{}:
			val = walkTo(v, path[1:])
		default:
			return nil
		}
	}

	return val
}
