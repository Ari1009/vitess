/*
Copyright 2023 The Vitess Authors.

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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/vt/log"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
)

/*
	This file introduces a new helper framework for vreplication tests. The current one uses a lot of globals
	and make assumptions which make adding new types of tests difficult.

	As part of a separate cleanup we will build on this framework to replace the existing one.
*/

var (
	seqVSchema = `{
		"sharded": false,
		"tables": {
			"customer_seq": {
				"type": "sequence"
			}
		}
	}`
	seqSchema       = `create table customer_seq(id int, next_id bigint, cache bigint, primary key(id)) comment 'vitess_sequence';`
	commerceSchema  = `create table customer(cid int, name varchar(128), ts timestamp(3) not null default current_timestamp(3), primary key(cid));`
	commerceVSchema = `
	{
	  "tables": {
		"customer": {}
	  }
	}
`
	customerSequenceVSchema = `
	{
	  "sharded": true,
	  "vindexes": {
		"reverse_bits": {
		  "type": "reverse_bits"
		}
	  },
	  "tables": {
		"customer": {
		  "column_vindexes": [
			{
			  "column": "cid",
			  "name": "reverse_bits"
			}
		  ],
		  "auto_increment": {
			"column": "cid",
			"sequence": "customer_seq"
		  }
		}
	  }
	}
	`
)

type keyspace struct {
	name    string
	vschema string
	schema  string
	baseID  int64
	shards  []string
}

type workflowOptions struct {
	tables       []string
	sourceShards []string
	targetShards []string
}

type workflow struct {
	name         string
	fromKeyspace string
	toKeyspace   string
	typ          string
	tc           *vrepTestCase
	options      *workflowOptions
}

type vrepTestCase struct {
	testName        string
	t               *testing.T
	defaultCellName string
	vtgateConn      *mysql.Conn
	keyspaces       map[string]*keyspace
	workflows       map[string]*workflow

	vc     *VitessCluster
	vtgate *cluster.VtgateProcess
}

func initPartialMoveTablesComplexTestCase(t *testing.T) *vrepTestCase {
	tc := &vrepTestCase{
		t:               t,
		testName:        t.Name(),
		keyspaces:       make(map[string]*keyspace),
		defaultCellName: "zone1",
		workflows:       make(map[string]*workflow),
	}
	tc.keyspaces["commerce"] = &keyspace{
		name:    "commerce",
		vschema: commerceVSchema,
		schema:  commerceSchema,
		baseID:  100,
		shards:  []string{"0"},
	}
	tc.keyspaces["customer"] = &keyspace{
		name:    "customer",
		vschema: customerSequenceVSchema,
		schema:  "",
		baseID:  200,
		shards:  []string{"-80", "80-"},
	}
	tc.keyspaces["customer2"] = &keyspace{
		name:    "customer2",
		vschema: customerSequenceVSchema,
		schema:  "",
		baseID:  1200,
		shards:  []string{"-80", "80-"},
	}
	tc.keyspaces["seqSrc"] = &keyspace{
		name:    "seqSrc",
		vschema: seqVSchema,
		schema:  seqSchema,
		baseID:  400,
		shards:  []string{"0"},
	}
	tc.keyspaces["seqTgt"] = &keyspace{
		name:    "seqTgt",
		vschema: "",
		schema:  "",
		baseID:  500,
		shards:  []string{"0"},
	}
	tc.setupCluster()
	tc.initData()
	return tc
}

func initSequenceResetTestCase(t *testing.T) *vrepTestCase {
	tc := &vrepTestCase{
		t:               t,
		testName:        t.Name(),
		keyspaces:       make(map[string]*keyspace),
		defaultCellName: "zone1",
		workflows:       make(map[string]*workflow),
	}
	tc.keyspaces["commerce"] = &keyspace{
		name:    "commerce",
		vschema: commerceVSchema,
		schema:  commerceSchema,
		baseID:  100,
		shards:  []string{"0"},
	}
	tc.keyspaces["customer"] = &keyspace{
		name:    "customer",
		vschema: customerSequenceVSchema,
		schema:  "",
		baseID:  200,
		shards:  []string{"-80", "80-"},
	}
	tc.keyspaces["seqSrc"] = &keyspace{
		name:    "seqSrc",
		vschema: seqVSchema,
		schema:  seqSchema,
		baseID:  400,
		shards:  []string{"0"},
	}
	tc.setupCluster()
	tc.initData()
	return tc
}

func (tc *vrepTestCase) teardown() {
	tc.vtgateConn.Close()
	vc.TearDown()
}

func (tc *vrepTestCase) setupCluster() {
	tc.vc = NewVitessCluster(tc.t, nil)
	vc = tc.vc // for backward compatibility since vc is used globally in this package
	require.NotNil(tc.t, tc.vc)
	tc.setupKeyspaces([]string{"commerce", "seqSrc"})
	tc.vtgateConn = getConnection(tc.t, tc.vc.ClusterConfig.hostname, tc.vc.ClusterConfig.vtgateMySQLPort)
}

func (tc *vrepTestCase) initData() {
	_, err := tc.vtgateConn.ExecuteFetch("insert into customer_seq(id, next_id, cache) values(0, 1000, 1000)", 1000, false)
	require.NoError(tc.t, err)
	_, err = tc.vtgateConn.ExecuteFetch("insert into customer(cid, name) values(1, 'customer1'), (2, 'customer2'),(3, 'customer3')", 1000, false)
	require.NoError(tc.t, err)
}

func (tc *vrepTestCase) setupKeyspaces(keyspaces []string) {
	for _, keyspace := range keyspaces {
		ks, ok := tc.keyspaces[keyspace]
		require.Equal(tc.t, true, ok, "keyspace %s not found", keyspace)
		tc.setupKeyspace(ks)
	}
}

func (tc *vrepTestCase) setupKeyspace(ks *keyspace) {
	t := tc.t
	if _, err := tc.vc.AddKeyspace(t, []*Cell{tc.vc.Cells["zone1"]}, ks.name, strings.Join(ks.shards, ","),
		ks.vschema, ks.schema, 0, 0, int(ks.baseID), nil); err != nil {
		t.Fatal(err)
	}
	if tc.vtgate == nil {
		defaultCellName := "zone1"
		defaultCell := tc.vc.Cells[defaultCellName]
		require.NotNil(tc.t, defaultCell)
		tc.vtgate = defaultCell.Vtgates[0]

	}
}

func (tc *vrepTestCase) newWorkflow(typ, workflowName, fromKeyspace, toKeyspace string, options *workflowOptions) *workflow {
	wf := &workflow{
		name:         workflowName,
		fromKeyspace: fromKeyspace,
		toKeyspace:   toKeyspace,
		typ:          typ,
		tc:           tc,
		options:      options,
	}
	return wf
}

func (wf *workflow) create() {
	var err error
	t := wf.tc.t
	typ := strings.ToLower(wf.typ)
	cell := wf.tc.defaultCellName
	switch typ {
	case "movetables":
		currentWorkflowType = binlogdatapb.VReplicationWorkflowType_MoveTables
		sourceShards := strings.Join(wf.options.sourceShards, ",")
		err = tstWorkflowExec(t, cell, wf.name, wf.fromKeyspace, wf.toKeyspace,
			strings.Join(wf.options.tables, ","), workflowActionCreate, "", sourceShards, "", defaultWorkflowExecOptions)
	case "reshard":
		currentWorkflowType = binlogdatapb.VReplicationWorkflowType_Reshard
		sourceShards := strings.Join(wf.options.sourceShards, ",")
		targetShards := strings.Join(wf.options.targetShards, ",")
		if targetShards == "" {
			targetShards = sourceShards
		}
		err = tstWorkflowExec(t, cell, wf.name, wf.fromKeyspace, wf.toKeyspace,
			strings.Join(wf.options.tables, ","), workflowActionCreate, "", sourceShards, targetShards, defaultWorkflowExecOptions)
	default:
		panic(fmt.Sprintf("unknown workflow type: %s", wf.typ))
	}
	require.NoError(t, err)
	waitForWorkflowState(t, wf.tc.vc, fmt.Sprintf("%s.%s", wf.toKeyspace, wf.name), binlogdatapb.VReplicationWorkflowState_Running.String())
	ks2 := wf.tc.vc.Cells[cell].Keyspaces[wf.toKeyspace]
	var i int64
	for _, shardName := range wf.tc.keyspaces[wf.toKeyspace].shards {
		tab := ks2.Shards[shardName].Tablets[fmt.Sprintf("%s-%d", cell, wf.tc.keyspaces[wf.toKeyspace].baseID+i)].Vttablet
		catchup(t, tab, wf.name, wf.typ)
		i += 100
	}
	doVtctldclientVDiff(t, wf.toKeyspace, wf.name, cell, nil)

}

func (wf *workflow) switchTraffic() {
	require.NoError(wf.tc.t, tstWorkflowExec(wf.tc.t, wf.tc.defaultCellName, wf.name, wf.fromKeyspace, wf.toKeyspace, "", workflowActionSwitchTraffic, "", "", "", defaultWorkflowExecOptions))
}

func (wf *workflow) reverseTraffic() {
	require.NoError(wf.tc.t, tstWorkflowExec(wf.tc.t, wf.tc.defaultCellName, wf.name, wf.fromKeyspace, wf.toKeyspace, "", workflowActionReverseTraffic, "", "", "", defaultWorkflowExecOptions))
}

func (wf *workflow) complete() {
	require.NoError(wf.tc.t, tstWorkflowExec(wf.tc.t, wf.tc.defaultCellName, wf.name, wf.fromKeyspace, wf.toKeyspace, "", workflowActionComplete, "", "", "", defaultWorkflowExecOptions))
}

// TestSequenceResetOnSwitchTraffic tests that in-memory sequence info is
// reset when switching traffic back and forth between keyspaces during
// MoveTables workflow. This catches a bug where cached sequence values would
// persist after traffic switches, causing sequence generation to produce
// duplicate values in target keyspace.
func TestSequenceResetOnSwitchTraffic(t *testing.T) {
	origExtraVTGateArgs := extraVTGateArgs
	extraVTGateArgs = append(extraVTGateArgs, []string{
		"--enable-partial-keyspace-migration",
		"--schema_change_signal=false",
	}...)
	defer func() {
		extraVTGateArgs = origExtraVTGateArgs
	}()

	tc := initSequenceResetTestCase(t)
	defer tc.teardown()

	currentCustomerCount = getCustomerCount(t, "")
	newCustomerCount = 4
	t.Run("Verify sequence reset during traffic switching", func(t *testing.T) {
		tc.setupKeyspaces([]string{"customer"})
		wf := tc.newWorkflow("MoveTables", "customer", "commerce", "customer", &workflowOptions{
			tables: []string{"customer"},
		})
		wf.create()

		vtgateConn, closeConn := getVTGateConn()
		defer closeConn()

		getSequenceNextID := func() int64 {
			qr := execVtgateQuery(t, vtgateConn, "", "SELECT next_id FROM seqSrc.customer_seq WHERE id = 0")
			nextID, _ := qr.Rows[0][0].ToInt64()
			return nextID
		}

		initialSeqValue := getSequenceNextID()
		t.Logf("Initial sequence next_id: %d", initialSeqValue)

		wf.switchTraffic()

		insertCustomers(t)

		afterFirstSwitchSeqValue := getSequenceNextID()
		t.Logf("After first switch sequence next_id: %d", afterFirstSwitchSeqValue)
		require.Greater(t, afterFirstSwitchSeqValue, initialSeqValue, "Sequence should increment after inserting customers")

		wf.reverseTraffic()

		afterReverseSeqValue := getSequenceNextID()
		t.Logf("After reverse switch sequence next_id: %d", afterReverseSeqValue)

		// Insert some random values when all writes are reversed back to
		// source keyspace. We are inserting here rows with IDs 1004, 1005,
		// 1006 (since the cache value was 1000) which would be the next
		// in-memory sequence IDs for inserting any new rows in `customer`
		// table if the sequence info isn't reset. This will result in
		// duplicate primary key value error in the next insert.
		//
		// Hence, this way we verify that even if there are any new
		// values inserted after the traffic has been reversed, the in-memory
		// sequence info is reset, so that on switching back the traffic to
		// the target keyspace the tablet refetches the next_id from the
		// sequence table for generating the next insert ID.
		_, err := tc.vtgateConn.ExecuteFetch("insert into customer(cid, name) values(1004, 'customer8'), (1005, 'customer9'),(1006, 'customer10')", 1000, false)
		require.NoError(t, err)
		_, err = tc.vtgateConn.ExecuteFetch("insert into customer(cid, name) values(2004, 'customer11'), (2005, 'customer12'),(2006, 'customer13')", 1000, false)
		require.NoError(t, err)

		wf.switchTraffic()

		afterSecondSwitchSeqValue := getSequenceNextID()
		// Since the highest ID before switching traffic was 2026, which is
		// greater than 2000 (the expected next_id from sequence table before switch.)
		assert.Equal(t, int64(2007), afterSecondSwitchSeqValue)

		currentCustomerCount = getCustomerCount(t, "after second switch")
		newCustomerCount = 4
		insertCustomers(t)

		finalSeqValue := getSequenceNextID()
		assert.Equal(t, int64(3007), finalSeqValue, "Since the cache is set to 1000, next_id is expected to be incremented to 3007")

		wf.complete()
	})
}

// TestPartialMoveTablesWithSequences enhances TestPartialMoveTables by adding an unsharded keyspace which has a
// sequence. This tests that the sequence is migrated correctly and that we can reverse traffic back to the source
func TestPartialMoveTablesWithSequences(t *testing.T) {
	origExtraVTGateArgs := extraVTGateArgs
	extraVTGateArgs = append(extraVTGateArgs, []string{
		"--enable-partial-keyspace-migration",
		"--schema_change_signal=false",
	}...)
	defer func() {
		extraVTGateArgs = origExtraVTGateArgs
	}()

	tc := initPartialMoveTablesComplexTestCase(t)
	defer tc.teardown()
	var err error

	t.Run("Move customer table from unsharded product keyspace to sharded customer keyspace.", func(t *testing.T) {
		tc.setupKeyspaces([]string{"customer"})
		wf := tc.newWorkflow("MoveTables", "customer", "commerce", "customer", &workflowOptions{
			tables: []string{"customer"},
		})
		wf.create()
		wf.switchTraffic()
		wf.complete()
	})

	var wfSeq *workflow
	t.Run("Start MoveTables for Sequence", func(t *testing.T) {
		tc.setupKeyspace(tc.keyspaces["seqTgt"])
		wfSeq = tc.newWorkflow("MoveTables", "seq", "seqSrc", "seqTgt", &workflowOptions{
			tables: []string{"customer_seq"},
		})
		wfSeq.create()
	})

	var emptyGlobalRoutingRules, emptyShardRoutingRules, preCutoverShardRoutingRules, halfCutoverShardRoutingRules, postCutoverShardRoutingRules string
	t.Run("Define and setup RoutingRules", func(t *testing.T) {
		emptyGlobalRoutingRules = "{}\n"

		// These should be listed in shard order
		emptyShardRoutingRules = `{"rules":[]}`
		preCutoverShardRoutingRules = `{"rules":[{"from_keyspace":"customer2","to_keyspace":"customer","shard":"-80"},{"from_keyspace":"customer2","to_keyspace":"customer","shard":"80-"}]}`
		halfCutoverShardRoutingRules = `{"rules":[{"from_keyspace":"customer2","to_keyspace":"customer","shard":"-80"},{"from_keyspace":"customer","to_keyspace":"customer2","shard":"80-"}]}`
		postCutoverShardRoutingRules = `{"rules":[{"from_keyspace":"customer","to_keyspace":"customer2","shard":"-80"},{"from_keyspace":"customer","to_keyspace":"customer2","shard":"80-"}]}`

		// Remove any manually applied shard routing rules as these
		// should be set by SwitchTraffic.
		applyShardRoutingRules(t, emptyShardRoutingRules)
		require.Equal(t, emptyShardRoutingRules, getShardRoutingRules(t))
	})

	wfName := "partial80Dash"
	sourceKs := "customer"
	targetKs := "customer2"
	shard := "80-"
	var wf80Dash, wfDash80 *workflow
	vtgateConn, closeConn := getVTGateConn()
	t.Run("Start MoveTables on customer2.80-", func(t *testing.T) {
		// Now setup the customer2 keyspace so we can do a partial move tables for one of the two shards: 80-.
		defaultRdonly = 0
		tc.setupKeyspaces([]string{"customer2"})
		wf80Dash = tc.newWorkflow("MoveTables", wfName, "customer", "customer2", &workflowOptions{
			sourceShards: []string{"80-"},
			tables:       []string{"customer"},
		})
		wf80Dash.create()

		waitForRowCount(t, vtgateConn, "customer2:80-", "customer", 2) // customer2: 80-
		waitForRowCount(t, vtgateConn, "customer", "customer", 3)      // customer: all shards
		waitForRowCount(t, vtgateConn, "customer2", "customer", 3)     // customer2: all shards
	})

	// This query uses an ID that should always get routed to shard 80-
	shard80DashRoutedQuery := "select name from customer where cid = 1 and noexistcol = 'foo'"
	// This query uses an ID that should always get routed to shard -80
	shardDash80RoutedQuery := "select name from customer where cid = 2 and noexistcol = 'foo'"

	// Reset any existing vtgate connection state.
	closeConn()

	vtgateConn, closeConn = getVTGateConn()
	defer closeConn()

	t.Run("Confirm routing rules", func(t *testing.T) {
		// Global routing rules should be in place with everything going to the source keyspace (customer).
		confirmGlobalRoutingToSource(t)

		// Shard routing rules should now also be in place with everything
		// going to the source keyspace (customer).
		require.Equal(t, preCutoverShardRoutingRules, getShardRoutingRules(t))

		// Confirm shard targeting works before we switch any traffic.
		// Everything should be routed to the source keyspace (customer).

		log.Infof("Testing reverse route (target->source) for shard being switched")
		_, err = vtgateConn.ExecuteFetch("use `customer2:80-`", 0, false)
		require.NoError(t, err)
		_, err = vtgateConn.ExecuteFetch(shard80DashRoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer.80-.primary", "Query was routed to the target before any SwitchTraffic")

		log.Infof("Testing reverse route (target->source) for shard NOT being switched")
		_, err = vtgateConn.ExecuteFetch("use `customer2:-80`", 0, false)
		require.NoError(t, err)
		_, err = vtgateConn.ExecuteFetch(shardDash80RoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer.-80.primary", "Query was routed to the target before any SwitchTraffic")

		_, err = vtgateConn.ExecuteFetch("use `customer`", 0, false) // switch vtgate default db back to customer
		require.NoError(t, err)

		// Switch all traffic for the shard
		wf80Dash.switchTraffic()
		expectedSwitchOutput := fmt.Sprintf("SwitchTraffic was successful for workflow %s.%s\n\nStart State: Reads Not Switched. Writes Not Switched\nCurrent State: Reads partially switched, for shards: %s. Writes partially switched, for shards: %s\n\n",
			targetKs, wfName, shard, shard)
		require.Equal(t, expectedSwitchOutput, lastOutput)

		// Confirm global routing rules -- everything should still be routed
		// to the source side, customer, globally.
		confirmGlobalRoutingToSource(t)

		// Confirm shard routing rules -- all traffic for the 80- shard should be
		// routed into the customer2 keyspace, overriding the global routing rules.
		require.Equal(t, halfCutoverShardRoutingRules, getShardRoutingRules(t))

		// Confirm global routing rules: -80 should still be be routed to customer
		// while 80- should be routed to customer2.
		require.Equal(t, halfCutoverShardRoutingRules, getShardRoutingRules(t))
	})
	vtgateConn.Close()
	vtgateConn = getConnection(t, tc.vc.ClusterConfig.hostname, tc.vc.ClusterConfig.vtgateMySQLPort)
	defer vtgateConn.Close()

	t.Run("Validate shard and tablet type routing", func(t *testing.T) {
		// No shard targeting
		_, err = vtgateConn.ExecuteFetch(shard80DashRoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer2.80-.primary", "Query was routed to the source after partial SwitchTraffic")
		_, err = vtgateConn.ExecuteFetch(shardDash80RoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer.-80.primary", "Query was routed to the target before partial SwitchTraffic")

		// Shard targeting
		_, err = vtgateConn.ExecuteFetch("use `customer2:80-`", 0, false)
		require.NoError(t, err)
		_, err = vtgateConn.ExecuteFetch(shard80DashRoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer2.80-.primary", "Query was routed to the source after partial SwitchTraffic")
		_, err = vtgateConn.ExecuteFetch("use `customer:80-`", 0, false)
		require.NoError(t, err)
		_, err = vtgateConn.ExecuteFetch(shard80DashRoutedQuery, 0, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "target: customer2.80-.primary", "Query was routed to the source after partial SwitchTraffic")

		_, err = vtgateConn.ExecuteFetch("use `customer`", 0, false) // switch vtgate default db back to customer
		require.NoError(t, err)
	})

	// Now move the other shard: -80
	t.Run("Move shard -80 and validate routing rules", func(t *testing.T) {
		// Now move the other shard: -80
		wfName = "partialDash80"
		shard = "-80"
		wfDash80 = tc.newWorkflow("MoveTables", wfName, "customer", "customer2", &workflowOptions{
			sourceShards: []string{"-80"},
			tables:       []string{"customer"},
		})
		wfDash80.create()
		wfDash80.switchTraffic()

		expectedSwitchOutput := fmt.Sprintf("SwitchTraffic was successful for workflow %s.%s\n\nStart State: Reads partially switched, for shards: 80-. Writes partially switched, for shards: 80-\nCurrent State: All Reads Switched. All Writes Switched\n\n",
			targetKs, wfName)
		require.Equal(t, expectedSwitchOutput, lastOutput)

		// Confirm global routing rules: everything should still be routed
		// to the source side, customer, globally.
		confirmGlobalRoutingToSource(t)

		// Confirm shard routing rules: all shards should be routed to the
		// target side (customer2).
		require.Equal(t, postCutoverShardRoutingRules, getShardRoutingRules(t))
	})

	var output string

	_, err = vtgateConn.ExecuteFetch("use `customer`", 0, false) // switch vtgate default db back to customer
	require.NoError(t, err)
	currentCustomerCount = getCustomerCount(t, "")
	t.Run("Switch sequence traffic forward and reverse and validate workflows still exist and sequence routing works", func(t *testing.T) {
		wfSeq.switchTraffic()
		log.Infof("SwitchTraffic was successful for workflow seqTgt.seq, with output %s", lastOutput)

		insertCustomers(t)

		wfSeq.reverseTraffic()
		log.Infof("ReverseTraffic was successful for workflow seqTgt.seq, with output %s", lastOutput)

		insertCustomers(t)

		wfSeq.switchTraffic()
		log.Infof("SwitchTraffic was successful for workflow seqTgt.seq, with output %s", lastOutput)

		insertCustomers(t)

		output, err = tc.vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", wfSeq.toKeyspace, "show", "--workflow", wfSeq.name)
		require.NoError(t, err)

		output, err = tc.vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", wfSeq.fromKeyspace, "show", "--workflow", fmt.Sprintf("%s_reverse", wfSeq.name))
		require.NoError(t, err)

		wfSeq.complete()
	})

	t.Run("Cancel reverse workflows and validate", func(t *testing.T) {
		// Cancel both reverse workflows (as we've done the cutover), which should
		// clean up both the global routing rules and the shard routing rules.
		for _, wf := range []string{"partialDash80", "partial80Dash"} {
			// We switched traffic, so it's the reverse workflow we want to cancel.
			reverseWf := wf + "_reverse"
			reverseKs := sourceKs // customer
			err = tstWorkflowExec(t, "", reverseWf, "", reverseKs, "", workflowActionCancel, "", "", "", defaultWorkflowExecOptions)
			require.NoError(t, err)

			output, err := tc.vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", reverseKs, "show", "--workflow", reverseWf)
			require.NoError(t, err)
			require.True(t, isEmptyWorkflowShowOutput(output))

			// Be sure that we've deleted the original workflow.
			_, _ = tc.vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", targetKs, "delete", "--workflow", wf)
			output, err = tc.vc.VtctldClient.ExecuteCommandWithOutput("Workflow", "--keyspace", targetKs, "show", "--workflow", wf)
			require.NoError(t, err)
			require.True(t, isEmptyWorkflowShowOutput(output))
		}

		// Confirm that the global routing rules are now gone.
		output, err = tc.vc.VtctldClient.ExecuteCommandWithOutput("GetRoutingRules", "--compact")
		require.NoError(t, err)
		require.Equal(t, emptyGlobalRoutingRules, output)

		// Confirm that the shard routing rules are now gone.
		require.Equal(t, emptyShardRoutingRules, getShardRoutingRules(t))
	})
}

var customerCount int64
var currentCustomerCount int64
var newCustomerCount = int64(201)
var lastCustomerId int64

func getCustomerCount(t *testing.T, msg string) int64 {
	vtgateConn, closeConn := getVTGateConn()
	defer closeConn()
	qr := execVtgateQuery(t, vtgateConn, "", "select count(*) from customer")
	require.NotNil(t, qr)
	count, err := qr.Rows[0][0].ToInt64()
	require.NoError(t, err)
	return count
}

func confirmLastCustomerIdHasIncreased(t *testing.T) {
	vtgateConn, closeConn := getVTGateConn()
	defer closeConn()
	qr := execVtgateQuery(t, vtgateConn, "", "select cid from customer order by cid desc limit 1")
	require.NotNil(t, qr)
	currentCustomerId, err := qr.Rows[0][0].ToInt64()
	require.NoError(t, err)
	require.Greater(t, currentCustomerId, lastCustomerId)
	lastCustomerId = currentCustomerId
}

func insertCustomers(t *testing.T) {
	vtgateConn, closeConn := getVTGateConn()
	defer closeConn()
	for i := int64(1); i < newCustomerCount+1; i++ {
		execVtgateQuery(t, vtgateConn, "customer@primary", fmt.Sprintf("insert into customer(name) values ('name-%d')", currentCustomerCount+i))
	}
	customerCount = getCustomerCount(t, "")
	require.Equal(t, currentCustomerCount+newCustomerCount, customerCount)
	currentCustomerCount = customerCount

	confirmLastCustomerIdHasIncreased(t)
}

func confirmGlobalRoutingToSource(t *testing.T) {
	output, err := vc.VtctldClient.ExecuteCommandWithOutput("GetRoutingRules", "--compact")
	require.NoError(t, err)
	result := gjson.Get(output, "rules")
	result.ForEach(func(attributeKey, attributeValue gjson.Result) bool {
		// 0 is the keyspace and 1 is optional tablename[@tablettype]
		fromKsTbl := strings.Split(attributeValue.Get("fromTable").String(), ".")
		// 0 is the keyspace and 1 is the tablename
		toKsTbl := strings.Split(attributeValue.Get("toTables.0").String(), ".")
		// All tables in the customer and customer2 keyspaces should be
		// routed to the customer keyspace.
		if fromKsTbl[0] == "customer" || fromKsTbl[0] == "customer2" {
			require.Equal(t, "customer", toKsTbl[0])
		}
		return true
	})
}
