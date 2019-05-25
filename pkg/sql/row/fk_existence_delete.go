// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package row

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
)

// fkExistenceCheckForDelete is an auxiliary object that facilitates the
// existence checks on the referencing table when deleting rows in a
// referenced table.
type fkExistenceCheckForDelete struct {
	// fks maps mutated index id to slice of fkExistenceCheckBaseHelper, which
	// performs FK existence checks in referencing tables.
	fks map[sqlbase.IndexID][]fkExistenceCheckBaseHelper

	// checker is the object that actually carries out the lookups in
	// KV.
	checker *fkExistenceBatchChecker
}

// makeFkExistenceCheckHelperForDelete instantiates a delete helper.
func makeFkExistenceCheckHelperForDelete(
	txn *client.Txn,
	table *sqlbase.ImmutableTableDescriptor,
	otherTables FkTableMetadata,
	colMap map[sqlbase.ColumnID]int,
	alloc *sqlbase.DatumAlloc,
) (fkExistenceCheckForDelete, error) {
	h := fkExistenceCheckForDelete{
		checker: &fkExistenceBatchChecker{
			txn: txn,
		},
	}

	// We need an existence check helper for every referencing
	// table.
	for _, ref := range table.InboundFKs {
		originTable := otherTables[ref.OriginTableID]
		if originTable.IsAdding {
			// We can assume that a table being added but not yet public is empty,
			// and thus does not need to be checked for FK violations.
			continue
		}
		// TODO(jordan,radu): this is busted, rip out when HP is removed.
		// Fake a forward foreign key constraint. The HP requires an index on the
		// reverse table, which won't be required by the CBO. So in HP, fail if we
		// don't have this precondition.
		var foundFK *sqlbase.ForeignKeyConstraint
		for _, fk := range originTable.Desc.OutboundFKs {
			if fk.ReferencedTableID != table.ID {
				continue
			}
			if sqlbase.ColumnIDs(fk.ReferencedColumnIDs).EqualSets(ref.ReferencedColumnIDs) {
				foundFK = fk
				break
			}
		}
		if foundFK == nil {
			return fkExistenceCheckForDelete{}, pgerror.AssertionFailedf(
				"failed to find forward fk for backward fk %v", ref)
		}
		fakeRef := &sqlbase.ForeignKeyConstraint{
			ReferencedColumnIDs: ref.OriginColumnIDs,
			OriginColumnIDs:     ref.ReferencedColumnIDs,
			ReferencedTableID:   ref.OriginTableID,
			Match:               foundFK.Match,
			OnDelete:            foundFK.OnDelete,
			OnUpdate:            foundFK.OnUpdate,
		}
		fk, err := makeFkExistenceCheckBaseHelper(txn, otherTables, fakeRef, colMap, alloc, CheckDeletes)
		if err == errSkipUnusedFK {
			continue
		}
		if err != nil {
			return fkExistenceCheckForDelete{}, err
		}
		if h.fks == nil {
			h.fks = make(map[sqlbase.IndexID][]fkExistenceCheckBaseHelper)
		}
		h.fks[0] = append(h.fks[0], fk)
	}

	return h, nil
}

// addAllIdxChecks queues a FK existence check for every referencing table.
func (h fkExistenceCheckForDelete) addAllIdxChecks(
	ctx context.Context, row tree.Datums, traceKV bool,
) error {
	for idx := range h.fks {
		if err := queueFkExistenceChecksForRow(ctx, h.checker, h.fks[idx], row, traceKV); err != nil {
			return err
		}
	}
	return nil
}
