/*
Copyright 2022 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vreplication

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/exp/maps"

	"github.com/buger/jsonparser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"vitess.io/vitess/go/json2"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/sqlescape"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/test/endtoend/throttler"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/throttle/throttlerapp"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

const (
	defaultTick          = 1 * time.Second
	defaultTimeout       = 60 * time.Second
	workflowStateTimeout = 90 * time.Second
)

func setSidecarDBName(dbName string) {
	sidecarDBName = dbName
	sidecarDBIdentifier = sqlparser.String(sqlparser.NewIdentifierCS(sidecarDBName))
}

func execMultipleQueries(t *testing.T, conn *mysql.Conn, database string, lines string) {
	queries := strings.Split(lines, "\n")
	for _, query := range queries {
		if strings.HasPrefix(query, "--") {
			continue
		}
		execVtgateQuery(t, conn, database, string(query))
	}
}

func execQueryWithRetry(t *testing.T, conn *mysql.Conn, query string, timeout time.Duration) *sqltypes.Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(defaultTick)
	defer ticker.Stop()

	var qr *sqltypes.Result
	var err error
	for {
		qr, err = conn.ExecuteFetch(query, 1000, false)
		if err == nil {
			return qr
		}
		select {
		case <-ctx.Done():
			require.FailNow(t, fmt.Sprintf("query %q did not succeed before the timeout of %s; last seen result: %v",
				query, timeout, qr))
		case <-ticker.C:
			log.Infof("query %q failed with error %v, retrying in %ds", query, err, defaultTick)
		}
	}
}

func execQuery(t *testing.T, conn *mysql.Conn, query string) *sqltypes.Result {
	qr, err := conn.ExecuteFetch(query, 1000, false)
	if err != nil {
		log.Errorf("Error executing query: %s: %v", query, err)
	}
	require.NoError(t, err)
	return qr
}
func getConnectionNoError(t *testing.T, hostname string, port int) *mysql.Conn {
	vtParams := mysql.ConnParams{
		Host:  hostname,
		Port:  port,
		Uname: "vt_dba",
	}
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	if err != nil {
		return nil
	}
	return conn
}

func getConnection(t *testing.T, hostname string, port int) *mysql.Conn {
	vtParams := mysql.ConnParams{
		Host:             hostname,
		Port:             port,
		Uname:            "vt_dba",
		ConnectTimeoutMs: 1000,
	}
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.NoErrorf(t, err, "error connecting to vtgate on %s:%d", hostname, port)
	return conn
}

func execVtgateQuery(t *testing.T, conn *mysql.Conn, database string, query string) *sqltypes.Result {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	if database != "" {
		execQuery(t, conn, "use `"+database+"`;")
	}
	execQuery(t, conn, "begin")
	qr := execQuery(t, conn, query)
	execQuery(t, conn, "commit")
	return qr
}

func checkHealth(t *testing.T, url string) bool {
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func waitForQueryResult(t *testing.T, conn *mysql.Conn, database string, query string, want string) {
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		qr := execVtgateQuery(t, conn, database, query)
		require.NotNil(t, qr)
		if want == fmt.Sprintf("%v", qr.Rows) {
			return
		}
		select {
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("query %q on database %q did not return the expected result of %v before the timeout of %s; last seen result: %v",
				query, database, want, defaultTimeout, qr.Rows))
		default:
			time.Sleep(defaultTick)
		}
	}
}

// waitForTabletThrottlingStatus waits for the tablet to return the provided HTTP code for
// the provided app name in its self check.
func waitForTabletThrottlingStatus(t *testing.T, tablet *cluster.VttabletProcess, throttlerApp throttlerapp.Name, wantCode tabletmanagerdatapb.CheckThrottlerResponseCode) bool {
	_, ok := throttler.WaitForCheckThrottlerResult(t, vc.VtctldClient, &cluster.Vttablet{Alias: tablet.Name}, throttlerApp, nil, wantCode, defaultTimeout)
	return ok
}

// waitForNoWorkflowLag waits for the VReplication workflow's MaxVReplicationTransactionLag
// value to be 0.
func waitForNoWorkflowLag(t *testing.T, vc *VitessCluster, keyspace, worfklow string) {
	ksWorkflow := fmt.Sprintf("%s.%s", keyspace, worfklow)
	lag := int64(0)
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		// We don't need log records for this so pass --include-logs=false.
		output, err := vc.VtctldClient.ExecuteCommandWithOutput("workflow", "--keyspace", keyspace, "show", "--workflow", worfklow, "--include-logs=false")
		require.NoError(t, err)
		// Confirm that we got no log records back.
		require.NotEmpty(t, len(gjson.Get(output, "workflows.0.shard_streams.*.streams.0").String()), "workflow %q had no streams listed in the output: %s", ksWorkflow, output)
		require.Equal(t, 0, len(gjson.Get(output, "workflows.0.shard_streams.*.streams.0.logs").Array()), "workflow %q returned log records when we expected none", ksWorkflow)
		lag = gjson.Get(output, "workflows.0.max_v_replication_lag").Int()
		if lag == 0 {
			return
		}
		select {
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("workflow %q did not eliminate VReplication lag before the timeout of %s; last seen MaxVReplicationTransactionLag: %d",
				ksWorkflow, defaultTimeout, lag))
		default:
			time.Sleep(defaultTick)
		}
	}
}

// verifyNoInternalTables can e.g. be used to confirm that no internal tables were
// copied from a source to a target during a MoveTables or Reshard operation.
func verifyNoInternalTables(t *testing.T, conn *mysql.Conn, keyspaceShard string) {
	qr := execVtgateQuery(t, conn, keyspaceShard, "show tables")
	require.NotNil(t, qr)
	require.NotNil(t, qr.Rows)
	for _, row := range qr.Rows {
		tableName := row[0].ToString()
		assert.False(t, schema.IsInternalOperationTableName(tableName), "found internal table %q in shard %q", tableName, keyspaceShard)
	}
}

func waitForRowCount(t *testing.T, conn *mysql.Conn, database string, table string, want int) {
	query := fmt.Sprintf("select count(*) from %s", table)
	wantRes := fmt.Sprintf("[[INT64(%d)]]", want)
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		qr := execVtgateQuery(t, conn, database, query)
		require.NotNil(t, qr)
		if wantRes == fmt.Sprintf("%v", qr.Rows) {
			return
		}
		select {
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("table %q did not reach the expected number of rows (%d) before the timeout of %s; last seen result: %v",
				table, want, defaultTimeout, qr.Rows))
		default:
			time.Sleep(defaultTick)
		}
	}
}

func waitForRowCountInTablet(t *testing.T, vttablet *cluster.VttabletProcess, database string, table string, want int64) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	ticker := time.NewTicker(defaultTick)
	defer ticker.Stop()

	query := fmt.Sprintf("select count(*) as c from %s", table)
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		qr, err := vttablet.QueryTablet(query, database, true)
		require.NoError(t, err)
		require.NotNil(t, qr)
		row := qr.Named().Row()
		require.NotNil(t, row)
		got := row.AsInt64("c", 0)
		require.LessOrEqual(t, got, want)
		if got == want {
			log.Infof("waitForRowCountInTablet: found %d rows in table %s on tablet %s", want, table, vttablet.Name)
			return
		}
		select {
		case <-ctx.Done():
			require.FailNow(
				t, fmt.Sprintf("table %q did not reach the expected number of rows (%d) on tablet %q before the timeout of %s; last seen result: %v",
					table, want, vttablet.Name, defaultTimeout, qr.Rows),
			)
			return
		case <-ticker.C:
		}
	}
}

// waitForSequenceValue queries the provided sequence name in the
// provided database using the provided vtgate connection until
// we get a next value from it. This allows us to move forward
// with queries that rely on the sequence working as expected.
// The read next value is also returned so that the caller can
// use it if they want.
// Note: you specify the number of values that you want to reserve
// and you get back the max value reserved.
func waitForSequenceValue(t *testing.T, conn *mysql.Conn, database, sequence string, numVals int) int64 {
	query := fmt.Sprintf("select next %d values from %s.%s", numVals, database, sequence)
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		qr, err := conn.ExecuteFetch(query, 1, false)
		if err == nil && qr != nil && len(qr.Rows) == 1 { // We got a value back
			val, err := qr.Rows[0][0].ToInt64()
			require.NoError(t, err, "invalid sequence value: %v", qr.Rows[0][0])
			return val
		}
		select {
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("sequence %q did not provide a next value before the timeout of %s; last seen result: %+v, error: %v",
				sequence, defaultTimeout, qr, err))
		default:
			time.Sleep(defaultTick)
		}
	}
}

func executeOnTablet(t *testing.T, conn *mysql.Conn, tablet *cluster.VttabletProcess, ksName string, query string, matchQuery string) (int, []byte, int, []byte) {
	queryStatsURL := fmt.Sprintf("http://%s:%d/debug/query_stats", tablet.TabletHostname, tablet.Port)

	count0, body0 := getQueryCount(t, queryStatsURL, matchQuery)

	qr := execVtgateQuery(t, conn, ksName, query)
	require.NotNil(t, qr)

	count1, body1 := getQueryCount(t, queryStatsURL, matchQuery)
	return count0, body0, count1, body1
}

func assertQueryExecutesOnTablet(t *testing.T, conn *mysql.Conn, tablet *cluster.VttabletProcess, ksName string, query string, matchQuery string) {
	t.Helper()
	rr, err := vc.VtctldClient.ExecuteCommandWithOutput("GetRoutingRules")
	require.NoError(t, err)
	count0, body0, count1, body1 := executeOnTablet(t, conn, tablet, ksName, query, matchQuery)
	require.Equalf(t, count0+1, count1, "query %q did not execute on destination %s (%s-%d);\ntried to match %q\nbefore:\n%s\n\nafter:\n%s\n\nrouting rules:\n%s\n\n",
		query, ksName, tablet.Cell, tablet.TabletUID, matchQuery, body0, body1, rr)
}

func assertQueryDoesNotExecutesOnTablet(t *testing.T, conn *mysql.Conn, tablet *cluster.VttabletProcess, ksName string, query string, matchQuery string) {
	t.Helper()
	count0, body0, count1, body1 := executeOnTablet(t, conn, tablet, ksName, query, matchQuery)
	assert.Equalf(t, count0, count1, "query %q executed in target;\ntried to match %q\nbefore:\n%s\n\nafter:\n%s\n\n", query, matchQuery, body0, body1)
}

func waitForWorkflowToBeCreated(t *testing.T, vc *VitessCluster, ksWorkflow string) {
	keyspace, workflow := parseKeyspaceWorkflow(t, ksWorkflow)
	require.NoError(t, waitForCondition("workflow to be created", func() bool {
		output, err := vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", keyspace, "show", "--workflow", workflow, "--compact", "--include-logs=false")
		return err == nil && !isEmptyWorkflowShowOutput(output)
	}, defaultTimeout))
}

// waitForWorkflowState waits for all of the given workflow's
// streams to reach the provided state. You can pass optional
// key value pairs of the form "key==value" to also wait for
// additional stream sub-state such as "message==for vdiff".
// Invalid checks are ignored.
func waitForWorkflowState(t *testing.T, vc *VitessCluster, ksWorkflow string, wantState string, fieldEqualityChecks ...string) {
	keyspace, workflow := parseKeyspaceWorkflow(t, ksWorkflow)
	done := false
	timer := time.NewTimer(workflowStateTimeout)
	defer timer.Stop()
	log.Infof("Waiting for workflow %q to fully reach %q state", ksWorkflow, wantState)
	for {
		output, err := vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", keyspace, "show", "--workflow", workflow, "--compact", "--include-logs=false")
		require.NoError(t, err, output)
		done = true
		state := ""
		shardStreams := gjson.Get(output, "workflows.0.shard_streams")
		// We need to wait for all streams in all shard streams to have the desired state.
		shardStreams.ForEach(func(shardStreamId, shardStream gjson.Result) bool {
			streams := shardStream.Get("*")
			streams.ForEach(func(streamId, stream gjson.Result) bool {
				info := stream.Map()
				state = info["state"].String()
				if state == wantState {
					for i := 0; i < len(fieldEqualityChecks); i++ {
						if kvparts := strings.Split(fieldEqualityChecks[i], "=="); len(kvparts) == 2 {
							key := kvparts[0]
							val := kvparts[1]
							res := info[key].String()
							if !strings.EqualFold(res, val) {
								done = false
							}
						}
					}
					if wantState == binlogdatapb.VReplicationWorkflowState_Running.String() &&
						(info["position"].Exists() && info["position"].String() == "") {
						done = false
					}
				} else {
					done = false
				}
				return true
			})
			return true
		})
		if done {
			log.Infof("Workflow %q has fully reached the desired state of %q", ksWorkflow, wantState)
			return
		}
		select {
		case <-timer.C:
			var extraRequirements string
			if len(fieldEqualityChecks) > 0 {
				extraRequirements = fmt.Sprintf(" with the additional requirements of \"%v\"", fieldEqualityChecks)
			}
			require.FailNowf(t, "workflow state not reached",
				"Workflow %q did not fully reach the expected state of %q%s before the timeout of %s; last seen output: %s",
				ksWorkflow, wantState, extraRequirements, workflowStateTimeout, output)
		default:
			time.Sleep(defaultTick)
		}
	}
}

// confirmTablesHaveSecondaryKeys confirms that the tables provided
// as a CSV have secondary keys. This is useful when testing the
// --defer-secondary-keys flag to confirm that the secondary keys
// were re-added by the time the workflow hits the running phase.
// For a Reshard workflow, where no tables are specified, pass
// an empty string for the tables and all tables in the target
// keyspace will be checked. It checks for the expected state until
// the timeout is reached.
func confirmTablesHaveSecondaryKeys(t *testing.T, tablets []*cluster.VttabletProcess, ksName string, tables string) {
	require.NotNil(t, tablets)
	require.NotNil(t, tablets[0])
	var tableArr []string
	if strings.TrimSpace(tables) != "" {
		tableArr = strings.Split(tables, ",")
	}
	if len(tableArr) == 0 { // We don't specify any for Reshard.
		// In this case we check all of them.
		res, err := tablets[0].QueryTablet("show tables", ksName, true)
		require.NoError(t, err)
		require.NotNil(t, res)
		for _, row := range res.Rows {
			tableArr = append(tableArr, row[0].ToString())
		}
	}
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		tablesWithoutSecondaryKeys := make([]string, 0)
		for _, tablet := range tablets {
			// Be sure that the schema is up to date.
			err := vc.VtctldClient.ExecuteCommand("ReloadSchema", topoproto.TabletAliasString(&topodatapb.TabletAlias{
				Cell: tablet.Cell,
				Uid:  uint32(tablet.TabletUID),
			}))
			require.NoError(t, err)
			for _, table := range tableArr {
				if schema.IsInternalOperationTableName(table) {
					continue
				}
				table := strings.TrimSpace(table)
				secondaryKeys := 0
				res, err := tablet.QueryTablet(fmt.Sprintf("show create table %s", sqlescape.EscapeID(table)), ksName, true)
				require.NoError(t, err)
				require.NotNil(t, res)
				row := res.Named().Row()
				tableSchema := row["Create Table"].ToString()
				parsedDDL, err := sqlparser.NewTestParser().ParseStrictDDL(tableSchema)
				require.NoError(t, err)
				createTable, ok := parsedDDL.(*sqlparser.CreateTable)
				require.True(t, ok)
				require.NotNil(t, createTable)
				require.NotNil(t, createTable.GetTableSpec())
				for _, index := range createTable.GetTableSpec().Indexes {
					if index.Info.Type != sqlparser.IndexTypePrimary {
						secondaryKeys++
					}
				}
				if secondaryKeys == 0 {
					tablesWithoutSecondaryKeys = append(tablesWithoutSecondaryKeys, table)
				}
			}
		}
		if len(tablesWithoutSecondaryKeys) == 0 {
			return
		}
		select {
		case <-timer.C:
			failureMessage := fmt.Sprintf("The following table(s) do not have any secondary keys: %s", strings.Join(tablesWithoutSecondaryKeys, ", "))
			require.FailNow(t, failureMessage)
		default:
			time.Sleep(defaultTick)
		}
	}
}

func getHTTPBody(t *testing.T, url string) []byte {
	resp, err := http.Get(url)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}

func getQueryCount(t *testing.T, url string, query string) (int, []byte) {
	body := getHTTPBody(t, url)

	var queryStats []struct {
		Query      string
		QueryCount uint64
	}

	err := json.Unmarshal(body, &queryStats)
	require.NoError(t, err)

	for _, q := range queryStats {
		if strings.Contains(q.Query, query) {
			return int(q.QueryCount), body
		}
	}

	return 0, body
}

func validateDryRunResults(t *testing.T, output string, want []string) {
	t.Helper()
	require.NotEmpty(t, output)
	gotDryRun := strings.Split(output, "\n")
	require.True(t, len(gotDryRun) > 3)
	var startRow int
	if strings.HasPrefix(gotDryRun[1], "Parameters:") { // vtctldclient
		startRow = 3
	} else if strings.Contains(gotDryRun[0], "deprecated") {
		startRow = 4
	} else {
		startRow = 2
	}
	gotDryRun = gotDryRun[startRow : len(gotDryRun)-1]
	if len(want) != len(gotDryRun) {
		require.Fail(t, "invalid dry run results", "want and got: lengths don't match, \nwant\n%s\n\ngot\n%s", strings.Join(want, "\n"), strings.Join(gotDryRun, "\n"))
	}
	var match, fail bool
	fail = false
	for i, w := range want {
		w = strings.TrimSpace(w)
		g := strings.TrimSpace(gotDryRun[i])
		if len(w) > 0 && w[0] == '/' {
			w = strings.TrimSpace(w[1:])
			result := strings.HasPrefix(g, w)
			match = result
		} else {
			match = g == w
		}
		if !match {
			fail = true
			require.Fail(t, "invlaid dry run results", "want %s, got %s\n", w, gotDryRun[i])
		}
	}
	if fail {
		require.Fail(t, "invalid dry run results", "Dry run results don't match, want %s, got %s", want, gotDryRun)
	}
}

func checkIfTableExists(t *testing.T, vc *VitessCluster, tabletAlias string, table string) (bool, error) {
	var output string
	var err error
	found := false

	if output, err = vc.VtctldClient.ExecuteCommandWithOutput("GetSchema", "--tables", table, tabletAlias); err != nil {
		return false, err
	}
	jsonparser.ArrayEach([]byte(output), func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		t, _ := jsonparser.GetString(value, "name")
		if t == table {
			found = true
		}
	}, "table_definitions")
	return found, nil
}

func validateTableInDenyList(t *testing.T, vc *VitessCluster, ksShard string, table string, mustExist bool) {
	found, err := isTableInDenyList(t, vc, ksShard, table)
	require.NoError(t, err)
	if mustExist {
		require.True(t, found, "Table %s not found in deny list", table)
	} else {
		require.False(t, found, "Table %s found in deny list", table)
	}
}

func isTableInDenyList(t *testing.T, vc *VitessCluster, ksShard string, table string) (bool, error) {
	output, err := vc.VtctldClient.ExecuteCommandWithOutput("GetShard", ksShard)
	require.NoError(t, err, "GetShard error", "%v %v", err, output)
	deniedTable := gjson.Get(output, fmt.Sprintf("shard.tablet_controls.0.denied_tables.#(==\"%s\"", table))
	return deniedTable.Exists(), nil
}

// expectNumberOfStreams waits for the given number of streams to be present and
// by default RUNNING. If you want to wait for different states, then you can
// pass in the state(s) you want to wait for.
func expectNumberOfStreams(t *testing.T, vtgateConn *mysql.Conn, name string, workflow string, database string, want int, states ...string) {
	var query string
	if len(states) == 0 {
		states = append(states, binlogdatapb.VReplicationWorkflowState_Running.String())
	}
	query = sqlparser.BuildParsedQuery("select count(*) from %s.vreplication where workflow='%s' and state in ('%s')",
		sidecarDBIdentifier, workflow, strings.Join(states, "','")).Query
	waitForQueryResult(t, vtgateConn, database, query, fmt.Sprintf(`[[INT64(%d)]]`, want))
}

// confirmAllStreamsRunning confirms that all of the workflow's streams are
// in the running state.
func confirmAllStreamsRunning(t *testing.T, vtgateConn *mysql.Conn, database string) {
	query := sqlparser.BuildParsedQuery("select count(*) from %s.vreplication where state != '%s'",
		sidecarDBIdentifier, binlogdatapb.VReplicationWorkflowState_Running.String()).Query
	waitForQueryResult(t, vtgateConn, database, query, `[[INT64(0)]]`)
}

func printShardPositions(vc *VitessCluster, ksShards []string) {
	for _, ksShard := range ksShards {
		output, err := vc.VtctldClient.ExecuteCommandWithOutput("ShardReplicationPositions", ksShard)
		if err != nil {
			fmt.Printf("Error in ShardReplicationPositions: %v, output %v", err, output)
		} else {
			fmt.Printf("Position of %s: %s", ksShard, output)
		}
	}
}

func printRoutingRules(t *testing.T, vc *VitessCluster, msg string) error {
	var output string
	var err error
	if output, err = vc.VtctldClient.ExecuteCommandWithOutput("GetRoutingRules", "--compact"); err != nil {
		return err
	}
	fmt.Printf("Routing Rules::%s:\n%s\n", msg, output)
	return nil
}

func osExec(t *testing.T, command string, args []string) (string, error) {
	cmd := exec.Command(command, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func getDebugVar(t *testing.T, port int, varPath []string) (string, error) {
	var val []byte
	var err error
	url := fmt.Sprintf("http://localhost:%d/debug/vars", port)
	log.Infof("url: %s, varPath: %s", url, strings.Join(varPath, ":"))
	body := getHTTPBody(t, url)
	val, _, _, err = jsonparser.Get(body, varPath...)
	require.NoError(t, err)
	return string(val), nil
}

func confirmWorkflowHasCopiedNoData(t *testing.T, targetKS, workflow string) {
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		output, err := vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", targetKs, "show", "--workflow", workflow, "--compact", "--include-logs=false")
		require.NoError(t, err, output)
		streams := gjson.Get(output, "workflows.0.shard_streams.*.streams")
		streams.ForEach(func(streamId, stream gjson.Result) bool { // For each stream
			info := stream.Map()
			state := info["state"]
			pos := info["position"]
			// If we've actually copied anything then we'll have a position in the stream
			if (state.Exists() && (state.String() == binlogdatapb.VReplicationWorkflowState_Running.String() || state.String() == binlogdatapb.VReplicationWorkflowState_Copying.String())) &&
				(pos.Exists() && pos.String() != "") {
				require.FailNowf(t, "Unexpected data copied in workflow",
					"The MoveTables workflow %q copied data in less than %s when it should have been waiting. Show output: %s",
					ksWorkflow, defaultTimeout, output)
			}
			return true
		})
		select {
		case <-timer.C:
			return
		default:
			time.Sleep(defaultTick)
		}
	}
}

// getShardRoutingRules returns the shard routing rules stored in the
// topo. It returns the rules sorted by shard,to_keyspace and with all
// newlines and whitespace removed so that we have predictable,
// compact, and easy to compare results for tests.
func getShardRoutingRules(t *testing.T) string {
	output, err := osExec(t, "vtctldclient", []string{"--server", getVtctldGRPCURL(), "GetShardRoutingRules"})
	log.Infof("GetShardRoutingRules err: %+v, output: %+v", err, output)
	require.Nilf(t, err, output)
	require.NotNil(t, output)

	// Sort the rules by shard,to_keyspace
	jsonOutput := gjson.Parse(output)
	rules := jsonOutput.Get("rules").Array()
	sort.Slice(rules, func(i, j int) bool {
		shardI := rules[i].Get("shard").String()
		shardJ := rules[j].Get("shard").String()
		if shardI == shardJ {
			return rules[i].Get("to_keyspace").String() < rules[j].Get("to_keyspace").String()
		}
		return shardI < shardJ
	})
	sb := strings.Builder{}
	for i := 0; i < len(rules); i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(rules[i].String())
	}
	output = fmt.Sprintf(`{"rules":[%s]}`, sb.String())

	// Remove newlines and whitespace
	re := regexp.MustCompile(`[\n\s]+`)
	output = re.ReplaceAllString(output, "")
	output = strings.TrimSpace(output)
	return output
}

func verifyCopyStateIsOptimized(t *testing.T, tablet *cluster.VttabletProcess) {
	// Update information_schem with the latest data
	_, err := tablet.QueryTablet(sqlparser.BuildParsedQuery("analyze table %s.copy_state", sidecarDBIdentifier).Query, "", false)
	require.NoError(t, err)

	// Verify that there's no delete marked rows and we reset the auto-inc value.
	// MySQL doesn't always immediately update information_schema so we wait.
	tmr := time.NewTimer(defaultTimeout)
	defer tmr.Stop()
	query := sqlparser.BuildParsedQuery("select data_free, auto_increment from information_schema.tables where table_schema='%s' and table_name='copy_state'", sidecarDBName).Query
	var dataFree, autoIncrement int64
	for {
		res, err := tablet.QueryTablet(query, "", false)
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Equal(t, 1, len(res.Rows))
		dataFree, err = res.Rows[0][0].ToInt64()
		require.NoError(t, err)
		autoIncrement, err = res.Rows[0][1].ToInt64()
		require.NoError(t, err)
		if dataFree == 0 && autoIncrement == 1 {
			return
		}

		select {
		case <-tmr.C:
			require.FailNowf(t, "timed out waiting for copy_state table to be optimized",
				"data_free should be 0 and auto_increment should be 1, last seen values were %d and %d respectively",
				dataFree, autoIncrement)
		default:
			time.Sleep(defaultTick)
		}
	}
}

// randHex can be used to generate random strings of
// hex characters to the given length. This can e.g.
// be used to generate and insert test data.
func randHex(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := crand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func getIntVal(t *testing.T, vars map[string]interface{}, key string) int {
	i, ok := vars[key].(float64)
	require.True(t, ok)
	return int(i)
}

func getPartialMetrics(t *testing.T, key string, tab *cluster.VttabletProcess) (int, int, int, int) {
	vars := tab.GetVars()
	insertKey := fmt.Sprintf("%s.insert", key)
	updateKey := fmt.Sprintf("%s.insert", key)
	cacheSizes := vars["VReplicationPartialQueryCacheSize"].(map[string]interface{})
	queryCounts := vars["VReplicationPartialQueryCount"].(map[string]interface{})
	if cacheSizes[insertKey] == nil || cacheSizes[updateKey] == nil ||
		queryCounts[insertKey] == nil || queryCounts[updateKey] == nil {
		return 0, 0, 0, 0
	}
	inserts := getIntVal(t, cacheSizes, insertKey)
	updates := getIntVal(t, cacheSizes, updateKey)
	insertQueries := getIntVal(t, queryCounts, insertKey)
	updateQueries := getIntVal(t, queryCounts, updateKey)
	return inserts, updates, insertQueries, updateQueries
}

// check that the connection's binlog row image is set to NOBLOB
func isBinlogRowImageNoBlob(t *testing.T, tablet *cluster.VttabletProcess) bool {
	rs, err := tablet.QueryTablet("select @@global.binlog_row_image", "", false)
	require.NoError(t, err)
	require.Equal(t, 1, len(rs.Rows))
	mode := strings.ToLower(rs.Rows[0][0].ToString())
	return mode == "noblob"
}

func getRowCount(t *testing.T, vtgateConn *mysql.Conn, table string) int {
	query := fmt.Sprintf("select count(*) from %s", table)
	qr := execQuery(t, vtgateConn, query)
	numRows, _ := qr.Rows[0][0].ToInt()
	return numRows
}

const (
	loadTestBufferingWindowDuration = 10 * time.Second
	loadTestAvgWaitBetweenQueries   = 500 * time.Microsecond
	loadTestDefaultConnections      = 100
)

type loadGenerator struct {
	t           *testing.T
	vc          *VitessCluster
	ctx         context.Context
	cancel      context.CancelFunc
	connections int
	wg          sync.WaitGroup
}

func newLoadGenerator(t *testing.T, vc *VitessCluster) *loadGenerator {
	return &loadGenerator{
		t:           t,
		vc:          vc,
		connections: loadTestDefaultConnections,
	}
}

func (lg *loadGenerator) stop() {
	// Wait for buffering to stop and additional records to be inserted by start
	// after traffic is switched.
	time.Sleep(loadTestBufferingWindowDuration * 2)
	log.Infof("Canceling load")
	lg.cancel()
	lg.wg.Wait()
}

func (lg *loadGenerator) start() {
	t := lg.t
	lg.ctx, lg.cancel = context.WithCancel(context.Background())
	var connectionCount atomic.Int64

	var id int64
	log.Infof("loadGenerator: starting")
	queryTemplate := "insert into loadtest(id, name) values (%d, 'name-%d')"
	var totalQueries, successfulQueries int64
	var deniedErrors, ambiguousErrors, reshardedErrors, tableNotFoundErrors, otherErrors int64
	lg.wg.Add(1)
	defer func() {
		defer lg.wg.Done()
		log.Infof("loadGenerator: totalQueries: %d, successfulQueries: %d, deniedErrors: %d, ambiguousErrors: %d, reshardedErrors: %d, tableNotFoundErrors: %d, otherErrors: %d",
			totalQueries, successfulQueries, deniedErrors, ambiguousErrors, reshardedErrors, tableNotFoundErrors, otherErrors)
	}()
	for {
		select {
		case <-lg.ctx.Done():
			log.Infof("loadGenerator: context cancelled")
			log.Infof("loadGenerator: deniedErrors: %d, ambiguousErrors: %d, reshardedErrors: %d, tableNotFoundErrors: %d, otherErrors: %d",
				deniedErrors, ambiguousErrors, reshardedErrors, tableNotFoundErrors, otherErrors)
			require.Equal(t, int64(0), deniedErrors)
			require.Equal(t, int64(0), otherErrors)
			require.Equal(t, int64(0), reshardedErrors)
			require.Equal(t, totalQueries, successfulQueries)
			return
		default:
			if int(connectionCount.Load()) < lg.connections {
				connectionCount.Add(1)
				lg.wg.Add(1)
				go func() {
					defer lg.wg.Done()
					defer connectionCount.Add(-1)
					conn := vc.GetVTGateConn(t)
					defer conn.Close()
					for {
						select {
						case <-lg.ctx.Done():
							return
						default:
						}
						newID := atomic.AddInt64(&id, 1)
						query := fmt.Sprintf(queryTemplate, newID, newID)
						_, err := conn.ExecuteFetch(query, 1, false)
						atomic.AddInt64(&totalQueries, 1)
						if err != nil {
							sqlErr := err.(*sqlerror.SQLError)
							if strings.Contains(strings.ToLower(err.Error()), "denied tables") {
								if debugMode {
									t.Logf("loadGenerator: denied tables error executing query: %d:%v", sqlErr.Number(), err)
								}
								atomic.AddInt64(&deniedErrors, 1)
							} else if strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
								// This can happen when a second keyspace is setup with the same tables, but
								// there are no routing rules set yet by MoveTables. So we ignore these errors.
								atomic.AddInt64(&ambiguousErrors, 1)
							} else if strings.Contains(strings.ToLower(err.Error()), "current keyspace is being resharded") {
								atomic.AddInt64(&reshardedErrors, 1)
							} else if strings.Contains(strings.ToLower(err.Error()), "not found") {
								atomic.AddInt64(&tableNotFoundErrors, 1)
							} else {
								if debugMode {
									t.Logf("loadGenerator: error executing query: %d:%v", sqlErr.Number(), err)
								}
								atomic.AddInt64(&otherErrors, 1)
							}
						} else {
							atomic.AddInt64(&successfulQueries, 1)
						}
						time.Sleep(time.Duration(int64(float64(loadTestAvgWaitBetweenQueries.Microseconds()) * rand.Float64())))
					}
				}()
			}
		}
	}
}

func (lg *loadGenerator) waitForCount(want int64) {
	t := lg.t
	conn := vc.GetVTGateConn(t)
	defer conn.Close()
	timer := time.NewTimer(defaultTimeout)
	defer timer.Stop()
	for {
		qr, err := conn.ExecuteFetch("select count(*) from loadtest", 1, false)
		require.NoError(t, err)
		require.NotNil(t, qr)
		got, _ := qr.Rows[0][0].ToInt64()

		if int64(got) >= want {
			return
		}
		select {
		case <-timer.C:
			require.FailNow(t, fmt.Sprintf("table %q did not reach the expected number of rows (%d) before the timeout of %s; last seen count: %v",
				"loadtest", want, defaultTimeout, got))
		default:
			time.Sleep(defaultTick)
		}
	}
}

// appendToQueryLog is useful when debugging tests.
func appendToQueryLog(msg string) {
	file, err := os.OpenFile(queryLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Errorf("Error opening query log file: %v", err)
		return
	}
	defer file.Close()
	if _, err := file.WriteString(msg + "\n"); err != nil {
		log.Errorf("Error writing to query log file: %v", err)
	}
}

func waitForCondition(name string, condition func() bool, timeout time.Duration) error {
	if condition() {
		return nil
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		select {
		case <-ticker.C:
			if condition() {
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("%s: waiting for %s", ctx.Err(), name)
		}
	}
}

func getCellNames(cells []*Cell) string {
	var cellNames []string
	if cells == nil {
		cells = []*Cell{}
		for _, cell := range vc.Cells {
			cells = append(cells, cell)
		}
	}
	for _, cell := range cells {
		cellNames = append(cellNames, cell.Name)
	}
	return strings.Join(cellNames, ",")
}

// VExplainPlan is the struct that represents the json output of a vexplain query.
type VExplainPlan struct {
	OperatorType string
	Variant      string
	Keyspace     VExplainKeyspace
	FieldQuery   string
	Query        string
	Table        string
}

type VExplainKeyspace struct {
	Name    string
	Sharded bool
}

// vexplain runs vexplain on the given query and returns the plan. Useful for validating routing rules.
func vexplain(t *testing.T, database, query string) *VExplainPlan {
	vtgateConn := vc.GetVTGateConn(t)
	defer vtgateConn.Close()

	qr := execVtgateQuery(t, vtgateConn, database, fmt.Sprintf("vexplain %s", query))
	require.NotNil(t, qr)
	require.Equal(t, 1, len(qr.Rows))
	json := qr.Rows[0][0].ToString()

	var plan VExplainPlan
	require.NoError(t, json2.Unmarshal([]byte(json), &plan))
	return &plan
}

// confirmKeyspacesRoutedTo confirms that the given keyspaces are routed as expected for the given tablet types, using vexplain.
func confirmKeyspacesRoutedTo(t *testing.T, keyspace string, routedKeyspace, table string, tabletTypes []string) {
	if len(tabletTypes) == 0 {
		tabletTypes = []string{"primary", "replica", "rdonly"}
	}
	for _, tt := range tabletTypes {
		database := fmt.Sprintf("%s@%s", keyspace, tt)
		plan := vexplain(t, database, fmt.Sprintf("select * from %s.%s", keyspace, table))
		require.Equalf(t, routedKeyspace, plan.Keyspace.Name, "for database %s, keyspace %v, tabletType %s", database, keyspace, tt)
	}
}

// confirmNoWorkflows confirms that there are no active workflows in the given keyspace.
func confirmNoWorkflows(t *testing.T, keyspace string) {
	output, err := vc.VtctldClient.ExecuteCommandWithOutput("GetWorkflows", keyspace, "--compact", "--include-logs=false")
	require.NoError(t, err)
	require.True(t, isEmptyWorkflowShowOutput(output))
}

// getVReplicationConfig returns the vreplication config for one random workflow for a given tablet. Currently, this is
// used when there is only one workflow, so we are using this simple method to get the config.
func getVReplicationConfig(t *testing.T, tab *cluster.VttabletProcess) map[string]string {
	configJson, err := getDebugVar(t, tab.Port, []string{"VReplicationConfig"})
	require.NoError(t, err)

	var config map[string]string
	err = json2.Unmarshal([]byte(configJson), &config)
	require.NoError(t, err)
	require.Equal(t, 1, len(config))

	configJson = config[maps.Keys(config)[0]]
	config = nil
	err = json2.Unmarshal([]byte(configJson), &config)
	require.NoError(t, err)

	return config
}

// mapToCSV converts a golang map to a CSV string for use in defining the config overrides in vrep CLI commands.
func mapToCSV(m map[string]string) string {
	csv := ""
	if len(m) == 0 {
		return csv
	}
	for k, v := range m {
		csv += fmt.Sprintf("%s=%s,", k, v)
	}
	if len(csv) == 0 {
		return csv
	}
	return csv[:len(csv)-1]
}

// validateOverrides validates that the given vttablets have the expected config overrides.
func validateOverrides(t *testing.T, tabs map[string]*cluster.VttabletProcess, want map[string]string) {
	for _, tab := range tabs {
		config := getVReplicationConfig(t, tab)
		for k, v := range want {
			require.EqualValues(t, v, config[k])
		}
	}
}

func parseKeyspaceWorkflow(t *testing.T, ksWorkflow string) (string, string) {
	t.Helper()
	keyspace, workflow, ok := strings.Cut(ksWorkflow, ".")
	require.True(t, ok, "invalid <keyspace>.<workflow> value: %s", ksWorkflow)
	return keyspace, workflow
}

func isEmptyWorkflowShowOutput(output string) bool {
	const (
		emptyJSON                           = `{}`
		emptyNonCompactWorkflowShowResponse = `{
  "workflows": []
}`
	)
	v := strings.TrimSpace(output)
	return v == emptyJSON || v == emptyNonCompactWorkflowShowResponse
}
