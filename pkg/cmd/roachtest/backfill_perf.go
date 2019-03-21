// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.

package main

import (
	"context"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"time"
)

func registerSchemaChangeBackfillPerf(r *registry) {
	r.Add(testSpec{
		Name:    "schemachange/backfillperf",
		Cluster: makeClusterSpec(5),
		Run: func(ctx context.Context, t *test, c *cluster) {
			runSchemaChangeBackfillPerf(ctx, t, c)
		},
	})
}

func runSchemaChangeBackfillPerf(ctx context.Context, t *test, c *cluster) {
	aNum := 50000000
	if c.isLocal() {
		aNum = 200000
	}
	bNum := 1
	cNum := 1

	crdbNodes := c.Range(1, c.nodes-1)
	workloadNode := c.Node(c.nodes)

	c.Put(ctx, cockroach, "./cockroach", crdbNodes)
	c.Put(ctx, workload, "./workload", workloadNode)
	c.Start(ctx, t, crdbNodes, startArgs("--env=COCKROACH_IMPORT_WORKLOAD_FASTER=true COCKROACH_LOG_SST_INFO_TICKS_INTERVAL=1"))

	cmdWrite := fmt.Sprintf(
		"./workload fixtures import bulkingest {pgurl:1} --a %d --b %d --c %d --index-b-c-a=false",
		aNum, bNum, cNum,
	)

	c.Run(ctx, workloadNode, cmdWrite)

	m := newMonitor(ctx, c, crdbNodes)

	indexDuration := time.Minute * 60
	if c.isLocal() {
		indexDuration = time.Minute
	}
	cmdWriteAndRead := fmt.Sprintf(
		"./workload run bulkingest --duration %s {pgurl:1-%d} --a %d --b %d --c %d",
		indexDuration.String(), c.nodes-1, aNum, bNum, cNum,
	)
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, workloadNode, cmdWriteAndRead)
		return nil
	})

	m.Go(func(ctx context.Context) error {
		db := c.Conn(ctx, 1)
		defer db.Close()

		pauseDuration := time.Minute * 5
		if c.isLocal() {
			pauseDuration = time.Second * 10
		}

		type index struct {
			add string
			drop string
		}
		stmts := []index{
			{
				`CREATE INDEX payload_a ON bulkingest.bulkingest (payload, a)`,
				`DROP INDEX bulkingest.bulkingest@payload_a`,
			},
			{
				`CREATE INDEX a_payload ON bulkingest.bulkingest (a, payload)`,
				`DROP INDEX bulkingest.bulkingest@a_payload`,
			},
		}

		for i, index := range stmts {
			// Let some traffic run before the schema change.
			time.Sleep(pauseDuration)
			c.l.Printf("running statement %d: %s\n", i+1, index.add)
			before := timeutil.Now()
			if _, err := db.Exec(index.add); err != nil {
				t.Fatal(err)
			}
			c.l.Printf("statement %s took %v\n", index.add, timeutil.Since(before))
			if _, err := db.Exec(index.drop); err != nil {
				t.Fatal(err)
			}
			c.l.Printf("ran statement %s", index.drop)
		}
		return nil
	})

	m.Wait()
}
