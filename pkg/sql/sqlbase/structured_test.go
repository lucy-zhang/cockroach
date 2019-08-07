// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sqlbase

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/gogo/protobuf/proto"
)

// Makes an IndexDescriptor with all columns being ascending.
func makeIndexDescriptor(name string, columnNames []string) IndexDescriptor {
	dirs := make([]IndexDescriptor_Direction, 0, len(columnNames))
	for range columnNames {
		dirs = append(dirs, IndexDescriptor_ASC)
	}
	idx := IndexDescriptor{
		ID:               IndexID(0),
		Name:             name,
		ColumnNames:      columnNames,
		ColumnDirections: dirs,
	}
	return idx
}

func TestAllocateIDs(t *testing.T) {
	defer leaktest.AfterTest(t)()

	desc := NewMutableCreatedTableDescriptor(TableDescriptor{
		ParentID: keys.MinUserDescID,
		ID:       keys.MinUserDescID + 1,
		Name:     "foo",
		Columns: []ColumnDescriptor{
			{Name: "a"},
			{Name: "b"},
			{Name: "c"},
		},
		PrimaryIndex: makeIndexDescriptor("c", []string{"a", "b"}),
		Indexes: []IndexDescriptor{
			makeIndexDescriptor("d", []string{"b", "a"}),
			makeIndexDescriptor("e", []string{"b"}),
		},
		Privileges:    NewDefaultPrivilegeDescriptor(),
		FormatVersion: FamilyFormatVersion,
	})
	if err := desc.AllocateIDs(); err != nil {
		t.Fatal(err)
	}

	expected := NewMutableCreatedTableDescriptor(TableDescriptor{
		ParentID: keys.MinUserDescID,
		ID:       keys.MinUserDescID + 1,
		Version:  1,
		Name:     "foo",
		Columns: []ColumnDescriptor{
			{ID: 1, Name: "a"},
			{ID: 2, Name: "b"},
			{ID: 3, Name: "c"},
		},
		Families: []ColumnFamilyDescriptor{
			{
				ID: 0, Name: "primary",
				ColumnNames:     []string{"a", "b", "c"},
				ColumnIDs:       []ColumnID{1, 2, 3},
				DefaultColumnID: 3,
			},
		},
		PrimaryIndex: IndexDescriptor{
			ID: 1, Name: "c", ColumnIDs: []ColumnID{1, 2},
			ColumnNames: []string{"a", "b"},
			ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC,
				IndexDescriptor_ASC}},
		Indexes: []IndexDescriptor{
			{ID: 2, Name: "d", ColumnIDs: []ColumnID{2, 1}, ColumnNames: []string{"b", "a"},
				ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC,
					IndexDescriptor_ASC}},
			{ID: 3, Name: "e", ColumnIDs: []ColumnID{2}, ColumnNames: []string{"b"},
				ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				ExtraColumnIDs:   []ColumnID{1}},
		},
		Privileges:     NewDefaultPrivilegeDescriptor(),
		NextColumnID:   4,
		NextFamilyID:   1,
		NextIndexID:    4,
		NextMutationID: 1,
		FormatVersion:  FamilyFormatVersion,
	})
	if !reflect.DeepEqual(expected, desc) {
		a, _ := json.MarshalIndent(expected, "", "  ")
		b, _ := json.MarshalIndent(desc, "", "  ")
		t.Fatalf("expected %s, but found %s", a, b)
	}

	if err := desc.AllocateIDs(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, desc) {
		a, _ := json.MarshalIndent(expected, "", "  ")
		b, _ := json.MarshalIndent(desc, "", "  ")
		t.Fatalf("expected %s, but found %s", a, b)
	}
}

func TestValidateTableDesc(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testData := []struct {
		err  string
		desc TableDescriptor
	}{
		{`empty table name`,
			TableDescriptor{}},
		{`invalid table ID 0`,
			TableDescriptor{ID: 0, Name: "foo"}},
		{`invalid parent ID 0`,
			TableDescriptor{ID: 2, Name: "foo"}},
		{`table "foo" is encoded using using version 0, but this client only supports version 2 and 3`,
			TableDescriptor{ID: 2, ParentID: 1, Name: "foo"}},
		{`table must contain at least 1 column`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
			}},
		{`empty column name`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 0},
				},
				NextColumnID: 2,
			}},
		{`invalid column ID 0`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 0, Name: "bar"},
				},
				NextColumnID: 2,
			}},
		{`table must contain a primary key`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`duplicate column name: "bar"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
					{ID: 1, Name: "bar"},
				},
				NextColumnID: 2,
			}},
		{`column "blah" duplicate ID of column "bar": 1`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
					{ID: 1, Name: "blah"},
				},
				NextColumnID: 2,
			}},
		{`at least 1 column family must be specified`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				NextColumnID: 2,
			}},
		{`the 0th family must have ID 0`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 1},
				},
				NextColumnID: 2,
			}},
		{`duplicate family name: "baz"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz"},
					{ID: 1, Name: "baz"},
				},
				NextColumnID: 2,
				NextFamilyID: 2,
			}},
		{`family "qux" duplicate ID of family "baz": 0`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz"},
					{ID: 0, Name: "qux"},
				},
				NextColumnID: 2,
				NextFamilyID: 2,
			}},
		{`duplicate family name: "baz"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz"},
					{ID: 3, Name: "baz"},
				},
				NextColumnID: 2,
				NextFamilyID: 2,
			}},
		{`mismatched column ID size (1) and name size (0)`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz", ColumnIDs: []ColumnID{1}},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`family "baz" contains unknown column "2"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz", ColumnIDs: []ColumnID{2}, ColumnNames: []string{"bar"}},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`family "baz" column 1 should have name "bar", but found name "qux"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"qux"}},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`column 1 is not in any column family`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz"},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`column 1 is in both family 0 and 1`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
					{ID: 1, Name: "qux", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				NextColumnID: 2,
				NextFamilyID: 2,
			}},
		{`primary key column 1 is not in column family 0`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "baz"},
					{ID: 1, Name: "qux", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "quux",
					ColumnIDs:        []ColumnID{1},
					ColumnNames:      []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				NextColumnID: 2,
				NextFamilyID: 2,
				NextIndexID:  2,
			}},
		{`table must contain a primary key`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{
					ID:               0,
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC}},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`invalid index ID 0`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 0, Name: "bar",
					ColumnIDs:        []ColumnID{0},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC}},
				NextColumnID: 2,
				NextFamilyID: 1,
			}},
		{`index "bar" must contain at least 1 column`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{
					ID: 1, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				Indexes: []IndexDescriptor{
					{ID: 2, Name: "bar"},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  3,
			}},
		{`mismatched column IDs (1) and names (0)`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar", ColumnIDs: []ColumnID{1}},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`mismatched column IDs (1) and names (2)`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
					{ID: 2, Name: "blah"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1, 2}, ColumnNames: []string{"bar", "blah"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar",
					ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar", "blah"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				NextColumnID: 3,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`duplicate index name: "bar"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar",
					ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				Indexes: []IndexDescriptor{
					{ID: 2, Name: "bar", ColumnIDs: []ColumnID{1},
						ColumnNames:      []string{"bar"},
						ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  3,
			}},
		{`index "blah" duplicate ID of index "bar": 1`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar", ColumnIDs: []ColumnID{1},
					ColumnNames:      []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				Indexes: []IndexDescriptor{
					{ID: 1, Name: "blah", ColumnIDs: []ColumnID{1},
						ColumnNames:      []string{"bar"},
						ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`index "bar" column "bar" should have ID 1, but found ID 2`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar", ColumnIDs: []ColumnID{2},
					ColumnNames:      []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`index "bar" contains unknown column "blah"`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar", ColumnIDs: []ColumnID{1},
					ColumnNames:      []string{"blah"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`mismatched column IDs (1) and directions (0)`,
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{ID: 1, Name: "bar", ColumnIDs: []ColumnID{1},
					ColumnNames: []string{"blah"},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  2,
			}},
		{`at least one of LIST or RANGE partitioning must be used`,
			// Verify that validatePartitioning is hooked up. The rest of these
			// tests are in TestValidatePartitionion.
			TableDescriptor{
				ID:            2,
				ParentID:      1,
				Name:          "foo",
				FormatVersion: FamilyFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "bar"},
				},
				Families: []ColumnFamilyDescriptor{
					{ID: 0, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"}},
				},
				PrimaryIndex: IndexDescriptor{
					ID: 1, Name: "primary", ColumnIDs: []ColumnID{1}, ColumnNames: []string{"bar"},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
					},
				},
				NextColumnID: 2,
				NextFamilyID: 1,
				NextIndexID:  3,
			}},
	}
	for i, d := range testData {
		if err := d.desc.ValidateTable(cluster.MakeTestingClusterSettings()); err == nil {
			t.Errorf("%d: expected \"%s\", but found success: %+v", i, d.err, d.desc)
		} else if d.err != err.Error() && "internal error: "+d.err != err.Error() {
			t.Errorf("%d: expected \"%s\", but found \"%+v\"", i, d.err, err)
		}
	}
}

func TestValidateCrossTableReferences(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	s, _, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	// TODO (jordan, lucy): upgrade these test cases to use the new representation.
	// Separately, write tests similar to these that exercise the upgrade and downgrade methods.

	tests := []struct {
		err        string
		desc       TableDescriptor
		otherDescs []TableDescriptor
	}{
		// Foreign keys
		{
			err: `invalid foreign key: missing table=52: descriptor not found`,
			desc: TableDescriptor{
				ID: 51,
				OutboundFKs: []ForeignKeyConstraint{
					{
						Name:                "fk",
						ReferencedTableID:   52,
						ReferencedColumnIDs: []ColumnID{1},
						OriginTableID:       51,
						OriginColumnIDs:     []ColumnID{1},
					},
				},
			},
			otherDescs: nil,
		},
		{
			err: `missing fk back reference fk to "foo" from "baz"`,
			desc: TableDescriptor{
				ID:   51,
				Name: "foo",
				OutboundFKs: []ForeignKeyConstraint{
					{
						Name:                "fk",
						ReferencedTableID:   52,
						ReferencedColumnIDs: []ColumnID{1},
						OriginTableID:       51,
						OriginColumnIDs:     []ColumnID{1},
					},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
			}},
		},
		{
			err: `invalid foreign key backreference: missing table=52: descriptor not found`,
			desc: TableDescriptor{
				ID: 51,
				InboundFKs: []ForeignKeyConstraint{
					{
						Name:                "fk",
						ReferencedTableID:   51,
						ReferencedColumnIDs: []ColumnID{1},
						OriginTableID:       52,
						OriginColumnIDs:     []ColumnID{1},
					},
				},
			},
		},
		{
			err: `missing fk forward reference fk to "foo" from "baz"`,
			desc: TableDescriptor{
				ID:   51,
				Name: "foo",
				PrimaryIndex: IndexDescriptor{
					ID:   1,
					Name: "bar",
				},
				InboundFKs: []ForeignKeyConstraint{
					{
						Name:                "fk",
						ReferencedTableID:   51,
						ReferencedColumnIDs: []ColumnID{1},
						OriginTableID:       52,
						OriginColumnIDs:     []ColumnID{1},
					},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
			}},
		},

		// Interleaves
		{
			err: `invalid interleave: missing table=52 index=2: descriptor not found`,
			desc: TableDescriptor{
				ID: 51,
				PrimaryIndex: IndexDescriptor{
					ID: 1,
					Interleave: InterleaveDescriptor{Ancestors: []InterleaveDescriptor_Ancestor{
						{TableID: 52, IndexID: 2},
					}},
				},
			},
			otherDescs: nil,
		},
		{
			err: `invalid interleave: missing table=baz index=2: index-id "2" does not exist`,
			desc: TableDescriptor{
				ID: 51,
				PrimaryIndex: IndexDescriptor{
					ID: 1,
					Interleave: InterleaveDescriptor{Ancestors: []InterleaveDescriptor_Ancestor{
						{TableID: 52, IndexID: 2},
					}},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
			}},
		},
		{
			err: `missing interleave back reference to "foo"@"bar" from "baz"@"qux"`,
			desc: TableDescriptor{
				ID:   51,
				Name: "foo",
				PrimaryIndex: IndexDescriptor{
					ID:   1,
					Name: "bar",
					Interleave: InterleaveDescriptor{Ancestors: []InterleaveDescriptor_Ancestor{
						{TableID: 52, IndexID: 2},
					}},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
				PrimaryIndex: IndexDescriptor{
					ID:   2,
					Name: "qux",
				},
			}},
		},
		{
			err: `invalid interleave backreference table=52 index=2: descriptor not found`,
			desc: TableDescriptor{
				ID: 51,
				PrimaryIndex: IndexDescriptor{
					ID:            1,
					InterleavedBy: []ForeignKeyReference{{Table: 52, Index: 2}},
				},
			},
		},
		{
			err: `invalid interleave backreference table=baz index=2: index-id "2" does not exist`,
			desc: TableDescriptor{
				ID: 51,
				PrimaryIndex: IndexDescriptor{
					ID:            1,
					InterleavedBy: []ForeignKeyReference{{Table: 52, Index: 2}},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
			}},
		},
		{
			err: `broken interleave backward reference from "foo"@"bar" to "baz"@"qux"`,
			desc: TableDescriptor{
				ID:   51,
				Name: "foo",
				PrimaryIndex: IndexDescriptor{
					ID:            1,
					Name:          "bar",
					InterleavedBy: []ForeignKeyReference{{Table: 52, Index: 2}},
				},
			},
			otherDescs: []TableDescriptor{{
				ID:   52,
				Name: "baz",
				PrimaryIndex: IndexDescriptor{
					ID:   2,
					Name: "qux",
				},
			}},
		},
	}

	{
		var v roachpb.Value
		desc := &Descriptor{Union: &Descriptor_Database{}}
		if err := v.SetProto(desc); err != nil {
			t.Fatal(err)
		}
		if err := kvDB.Put(ctx, MakeDescMetadataKey(0), &v); err != nil {
			t.Fatal(err)
		}
	}

	for i, test := range tests {
		for _, otherDesc := range test.otherDescs {
			otherDesc.Privileges = NewDefaultPrivilegeDescriptor()
			var v roachpb.Value
			desc := &Descriptor{Union: &Descriptor_Table{Table: &otherDesc}}
			if err := v.SetProto(desc); err != nil {
				t.Fatal(err)
			}
			if err := kvDB.Put(ctx, MakeDescMetadataKey(otherDesc.ID), &v); err != nil {
				t.Fatal(err)
			}
		}
		txn := client.NewTxn(ctx, kvDB, s.NodeID(), client.RootTxn)
		if err := test.desc.validateCrossReferences(ctx, txn); err == nil {
			t.Errorf("%d: expected \"%s\", but found success: %+v", i, test.err, test.desc)
		} else if test.err != err.Error() && "internal error: "+test.err != err.Error() {
			t.Errorf("%d: expected \"%s\", but found \"%s\"", i, test.err, err.Error())
		}
		for _, otherDesc := range test.otherDescs {
			if err := kvDB.Del(ctx, MakeDescMetadataKey(otherDesc.ID)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestValidatePartitioning(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tests := []struct {
		err  string
		desc TableDescriptor
	}{
		{"at least one of LIST or RANGE partitioning must be used",
			TableDescriptor{
				PrimaryIndex: IndexDescriptor{
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
					},
				},
			},
		},
		{"PARTITION p1: must contain values",
			TableDescriptor{
				PrimaryIndex: IndexDescriptor{
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List:       []PartitioningDescriptor_List{{Name: "p1"}},
					},
				},
			},
		},
		{"not enough columns in index for this partitioning",
			TableDescriptor{
				PrimaryIndex: IndexDescriptor{
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List:       []PartitioningDescriptor_List{{Name: "p1", Values: [][]byte{{}}}},
					},
				},
			},
		},
		{"only one LIST or RANGE partitioning may used",
			TableDescriptor{
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List:       []PartitioningDescriptor_List{{}},
						Range:      []PartitioningDescriptor_Range{{}},
					},
				},
			},
		},
		{"PARTITION name must be non-empty",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List:       []PartitioningDescriptor_List{{}},
					},
				},
			},
		},
		{"PARTITION p1: must contain values",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List:       []PartitioningDescriptor_List{{Name: "p1"}},
					},
				},
			},
		},
		{"PARTITION p1: decoding: empty array",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{{
							Name: "p1", Values: [][]byte{{}},
						}},
					},
				},
			},
		},
		{"PARTITION p1: decoding: int64 varint decoding failed: 0",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{
							{Name: "p1", Values: [][]byte{{0x03}}},
						},
					},
				},
			},
		},
		{"PARTITION p1: superfluous data in encoded value",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{
							{Name: "p1", Values: [][]byte{{0x03, 0x02, 0x00}}},
						},
					},
				},
			},
		},
		{"partitions p1 and p2 overlap",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1, 1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC, IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						Range: []PartitioningDescriptor_Range{
							{Name: "p1", FromInclusive: []byte{0x03, 0x02}, ToExclusive: []byte{0x03, 0x04}},
							{Name: "p2", FromInclusive: []byte{0x03, 0x02}, ToExclusive: []byte{0x03, 0x04}},
						},
					},
				},
			},
		},
		{"PARTITION p1: name must be unique",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{
							{Name: "p1", Values: [][]byte{{0x03, 0x02}}},
							{Name: "p1", Values: [][]byte{{0x03, 0x04}}},
						},
					},
				},
			},
		},
		{"not enough columns in index for this partitioning",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{{
							Name:   "p1",
							Values: [][]byte{{0x03, 0x02}},
							Subpartitioning: PartitioningDescriptor{
								NumColumns: 1,
								List:       []PartitioningDescriptor_List{{Name: "p1_1", Values: [][]byte{{}}}},
							},
						}},
					},
				},
			},
		},
		{"PARTITION p1: name must be unique",
			TableDescriptor{
				Columns: []ColumnDescriptor{{ID: 1, Type: *types.Int}},
				PrimaryIndex: IndexDescriptor{
					ColumnIDs:        []ColumnID{1, 1},
					ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC, IndexDescriptor_ASC},
					Partitioning: PartitioningDescriptor{
						NumColumns: 1,
						List: []PartitioningDescriptor_List{
							{Name: "p1", Values: [][]byte{{0x03, 0x02}}},
							{
								Name:   "p2",
								Values: [][]byte{{0x03, 0x04}},
								Subpartitioning: PartitioningDescriptor{
									NumColumns: 1,
									List: []PartitioningDescriptor_List{
										{Name: "p1", Values: [][]byte{{0x03, 0x02}}},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	for i, test := range tests {
		err := test.desc.validatePartitioning()
		if !testutils.IsError(err, test.err) {
			t.Errorf(`%d: got "%v" expected "%v"`, i, err, test.err)
		}
	}
}

func TestColumnTypeSQLString(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testData := []struct {
		colType     *types.T
		expectedSQL string
	}{
		{types.MakeBit(2), "BIT(2)"},
		{types.MakeVarBit(2), "VARBIT(2)"},
		{types.Int, "INT8"},
		{types.Float, "FLOAT8"},
		{types.Float4, "FLOAT4"},
		{types.Decimal, "DECIMAL"},
		{types.MakeDecimal(6, 0), "DECIMAL(6)"},
		{types.MakeDecimal(8, 7), "DECIMAL(8,7)"},
		{types.Date, "DATE"},
		{types.Timestamp, "TIMESTAMP"},
		{types.Interval, "INTERVAL"},
		{types.String, "STRING"},
		{types.MakeString(10), "STRING(10)"},
		{types.Bytes, "BYTES"},
	}
	for i, d := range testData {
		t.Run(d.colType.DebugString(), func(t *testing.T) {
			sql := d.colType.SQLString()
			if d.expectedSQL != sql {
				t.Errorf("%d: expected %s, but got %s", i, d.expectedSQL, sql)
			}
		})
	}
}

func TestFitColumnToFamily(t *testing.T) {
	intEncodedSize := 10 // 1 byte tag + 9 bytes max varint encoded size

	makeTestTableDescriptor := func(familyTypes [][]types.T) *MutableTableDescriptor {
		nextColumnID := ColumnID(8)
		var desc TableDescriptor
		for _, fTypes := range familyTypes {
			var family ColumnFamilyDescriptor
			for _, t := range fTypes {
				desc.Columns = append(desc.Columns, ColumnDescriptor{
					ID:   nextColumnID,
					Type: t,
				})
				family.ColumnIDs = append(family.ColumnIDs, nextColumnID)
				nextColumnID++
			}
			desc.Families = append(desc.Families, family)
		}
		return NewMutableCreatedTableDescriptor(desc)
	}

	emptyFamily := []types.T{}
	partiallyFullFamily := []types.T{
		*types.Int,
		*types.Bytes,
	}
	fullFamily := []types.T{
		*types.Bytes,
	}
	maxIntsInOneFamily := make([]types.T, FamilyHeuristicTargetBytes/intEncodedSize)
	for i := range maxIntsInOneFamily {
		maxIntsInOneFamily[i] = *types.Int
	}

	tests := []struct {
		newCol           types.T
		existingFamilies [][]types.T
		colFits          bool
		idx              int // not applicable if colFits is false
	}{
		// Bounded size column.
		{colFits: true, idx: 0, newCol: *types.Bool,
			existingFamilies: nil,
		},
		{colFits: true, idx: 0, newCol: *types.Bool,
			existingFamilies: [][]types.T{emptyFamily},
		},
		{colFits: true, idx: 0, newCol: *types.Bool,
			existingFamilies: [][]types.T{partiallyFullFamily},
		},
		{colFits: true, idx: 0, newCol: *types.Bool,
			existingFamilies: [][]types.T{fullFamily},
		},
		{colFits: true, idx: 0, newCol: *types.Bool,
			existingFamilies: [][]types.T{fullFamily, emptyFamily},
		},

		// Unbounded size column.
		{colFits: true, idx: 0, newCol: *types.Decimal,
			existingFamilies: [][]types.T{emptyFamily},
		},
		{colFits: true, idx: 0, newCol: *types.Decimal,
			existingFamilies: [][]types.T{partiallyFullFamily},
		},
	}
	for i, test := range tests {
		desc := makeTestTableDescriptor(test.existingFamilies)
		idx, colFits := fitColumnToFamily(desc, ColumnDescriptor{Type: test.newCol})
		if colFits != test.colFits {
			if colFits {
				t.Errorf("%d: expected no fit for the column but got one", i)
			} else {
				t.Errorf("%d: expected fit for the column but didn't get one", i)
			}
			continue
		}
		if colFits && idx != test.idx {
			t.Errorf("%d: got a fit in family offset %d but expected offset %d", i, idx, test.idx)
		}
	}
}

func TestMaybeUpgradeFormatVersion(t *testing.T) {
	tests := []struct {
		desc       TableDescriptor
		expUpgrade bool
		verify     func(int, TableDescriptor) // nil means no extra verification.
	}{
		{
			desc: TableDescriptor{
				FormatVersion: BaseFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "foo"},
				},
			},
			expUpgrade: true,
			verify: func(i int, desc TableDescriptor) {
				if len(desc.Families) == 0 {
					t.Errorf("%d: expected families to be set, but it was empty", i)
				}
			},
		},
		// Test that a version from the future is left alone.
		{
			desc: TableDescriptor{
				FormatVersion: InterleavedFormatVersion,
				Columns: []ColumnDescriptor{
					{ID: 1, Name: "foo"},
				},
			},
			expUpgrade: false,
			verify:     nil,
		},
	}
	for i, test := range tests {
		desc := test.desc
		upgraded := desc.maybeUpgradeFormatVersion()
		if upgraded != test.expUpgrade {
			t.Fatalf("%d: expected upgraded=%t, but got upgraded=%t", i, test.expUpgrade, upgraded)
		}
		if test.verify != nil {
			test.verify(i, desc)
		}
	}
}

func TestUnvalidateConstraints(t *testing.T) {
	desc := NewMutableCreatedTableDescriptor(TableDescriptor{
		Name:          "test",
		ParentID:      ID(1),
		Columns:       []ColumnDescriptor{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		FormatVersion: FamilyFormatVersion,
		Indexes:       []IndexDescriptor{makeIndexDescriptor("d", []string{"b", "a"})},
		Privileges:    NewDefaultPrivilegeDescriptor(),
		OutboundFKs: []ForeignKeyConstraint{
			{
				Name:              "fk",
				ReferencedTableID: ID(1),
				Validity:          ConstraintValidity_Validated,
			},
		},
	})
	if err := desc.AllocateIDs(); err != nil {
		t.Fatal(err)
	}
	lookup := func(_ ID) (*TableDescriptor, error) {
		return desc.TableDesc(), nil
	}

	before, err := desc.GetConstraintInfoWithLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := before["fk"]; !ok || c.Unvalidated {
		t.Fatalf("expected to find a validated constraint fk before, found %v", c)
	}
	desc.InvalidateFKConstraints()

	after, err := desc.GetConstraintInfoWithLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := after["fk"]; !ok || !c.Unvalidated {
		t.Fatalf("expected to find an unvalidated constraint fk before, found %v", c)
	}
}

func TestKeysPerRow(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// TODO(dan): This server is only used to turn a CREATE TABLE statement into
	// a TableDescriptor. It should be possible to move MakeTableDesc into
	// sqlbase. If/when that happens, use it here instead of this server.
	s, conn, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())
	if _, err := conn.Exec(`CREATE DATABASE d`); err != nil {
		t.Fatalf("%+v", err)
	}

	tests := []struct {
		createTable string
		indexID     IndexID
		expected    int
	}{
		{"(a INT PRIMARY KEY, b INT, INDEX (b))", 1, 1},                         // Primary index
		{"(a INT PRIMARY KEY, b INT, INDEX (b))", 2, 1},                         // 'b' index
		{"(a INT PRIMARY KEY, b INT, FAMILY (a), FAMILY (b), INDEX (b))", 1, 2}, // Primary index
		{"(a INT PRIMARY KEY, b INT, FAMILY (a), FAMILY (b), INDEX (b))", 2, 1}, // 'b' index
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("%s - %d", test.createTable, test.indexID), func(t *testing.T) {
			sqlDB := sqlutils.MakeSQLRunner(conn)
			tableName := fmt.Sprintf("t%d", i)
			sqlDB.Exec(t, fmt.Sprintf(`CREATE TABLE d.%s %s`, tableName, test.createTable))

			var descBytes []byte
			// Grab the most recently created descriptor.
			row := sqlDB.QueryRow(t,
				`SELECT descriptor FROM system.descriptor ORDER BY id DESC LIMIT 1`)
			row.Scan(&descBytes)
			var desc Descriptor
			if err := protoutil.Unmarshal(descBytes, &desc); err != nil {
				t.Fatalf("%+v", err)
			}

			keys := desc.GetTable().KeysPerRow(test.indexID)
			if test.expected != keys {
				t.Errorf("expected %d keys got %d", test.expected, keys)
			}
		})
	}
}

func TestColumnNeedsBackfill(t *testing.T) {
	// Define variable strings here such that we can pass their address below
	null := "NULL"
	four := "4:::INT8"
	// Create Column Descriptors that reflect the definition of a column with a
	// default value of NULL that was set implicitly, one that was set explicitly,
	// and one that has an INT default value, respectively.
	implicitNull := &ColumnDescriptor{Name: "im", ID: 2, DefaultExpr: nil, Nullable: true, ComputeExpr: nil}
	explicitNull := &ColumnDescriptor{Name: "ex", ID: 3, DefaultExpr: &null, Nullable: true, ComputeExpr: nil}
	defaultNotNull := &ColumnDescriptor{Name: "four", ID: 4, DefaultExpr: &four, Nullable: true, ComputeExpr: nil}
	// Verify that a backfill doesn't occur according to the ColumnNeedsBackfill
	// function for the default NULL values, and that it does occur for an INT
	// default value.
	if ColumnNeedsBackfill(implicitNull) != false {
		t.Fatal("Expected implicit SET DEFAULT NULL to not require a backfill," +
			" ColumnNeedsBackfill states that it does.")
	}
	if ColumnNeedsBackfill(explicitNull) != false {
		t.Fatal("Expected explicit SET DEFAULT NULL to not require a backfill," +
			" ColumnNeedsBackfill states that it does.")
	}
	if ColumnNeedsBackfill(defaultNotNull) != true {
		t.Fatal("Expected explicit SET DEFAULT NULL to require a backfill," +
			" ColumnNeedsBackfill states that it does not.")
	}
}

type fakeProtoGetter struct {
	protos map[interface{}]protoutil.Message
}

func (m fakeProtoGetter) GetProto(
	ctx context.Context, key interface{}, msg protoutil.Message,
) error {
	msg.Reset()
	if other, ok := m.protos[string(key.(roachpb.Key))]; ok {
		bytes := make([]byte, other.Size())
		if _, err := other.MarshalTo(bytes); err != nil {
			return err
		}
		if err := protoutil.Unmarshal(bytes, msg); err != nil {
			return err
		}
	}
	return nil
}

// oldFormatUpgradedPair is a helper struct for the upgrade/downgrade test
// below. It holds an "old format" (pre-19.2) table descriptor, and an expected
// upgraded equivalent. The test will verify that the old format descriptor
// upgrades into the provided expected descriptor.
type oldFormatUpgradedPair struct {
	oldFormat        TableDescriptor
	expectedUpgraded TableDescriptor
}

// This test exercises the foreign key representation upgrade and downgrade
// methods that were introduced in 19.2 to move foreign key descriptors from
// the index descriptor representation onto the new table descriptor
// representation.
func TestUpgradeDowngradeFKRepr(t *testing.T) {
	mixedVersionSettings := cluster.MakeTestingClusterSettingsWithVersion(
		cluster.BinaryMinimumSupportedVersion,
		cluster.VersionByKey(cluster.VersionTopLevelForeignKeys-1),
	)
	newVersionSettings := cluster.MakeTestingClusterSettingsWithVersion(
		cluster.BinaryMinimumSupportedVersion,
		cluster.VersionByKey(cluster.VersionTopLevelForeignKeys),
	)

	testCases := []struct {
		name       string
		origin     oldFormatUpgradedPair
		referenced oldFormatUpgradedPair
	}{
		0: {
			name: "simple",
			origin: oldFormatUpgradedPair{
				oldFormat: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ID:        1,
							ColumnIDs: ColumnIDs{1},
							ForeignKey: ForeignKeyReference{
								Table:           2,
								Index:           2,
								Name:            "foo",
								Validity:        ConstraintValidity_Validating,
								SharedPrefixLen: 1,
								OnDelete:        ForeignKeyReference_NO_ACTION,
								OnUpdate:        ForeignKeyReference_NO_ACTION,
								Match:           ForeignKeyReference_SIMPLE,
							},
						},
					},
				},
				expectedUpgraded: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ID:        1,
							ColumnIDs: ColumnIDs{1},
						},
					},
					OutboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   2,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
					},
				},
			},
			referenced: oldFormatUpgradedPair{
				oldFormat: TableDescriptor{
					ID:      2,
					Columns: []ColumnDescriptor{{ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ColumnIDs: ColumnIDs{2},
							ID:        2,
							ReferencedBy: []ForeignKeyReference{
								{
									Table: 1,
									Index: 1,
								},
							},
						},
					},
				},
				expectedUpgraded: TableDescriptor{
					ID:      2,
					Columns: []ColumnDescriptor{{ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ColumnIDs: ColumnIDs{2},
							ID:        2,
						},
					},
					InboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   2,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
					},
				},
			},
		},
		1: {
			name: "primaryKey",
			origin: oldFormatUpgradedPair{
				oldFormat: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					PrimaryIndex: IndexDescriptor{
						ID:        1,
						ColumnIDs: ColumnIDs{1},
						ForeignKey: ForeignKeyReference{
							Table:           2,
							Index:           2,
							Name:            "foo",
							Validity:        ConstraintValidity_Validating,
							SharedPrefixLen: 1,
							OnDelete:        ForeignKeyReference_NO_ACTION,
							OnUpdate:        ForeignKeyReference_NO_ACTION,
							Match:           ForeignKeyReference_SIMPLE,
						},
					},
				},
				expectedUpgraded: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					PrimaryIndex: IndexDescriptor{
						ID:        1,
						ColumnIDs: ColumnIDs{1},
					},
					OutboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   2,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
					},
				},
			},
			referenced: oldFormatUpgradedPair{
				oldFormat: TableDescriptor{
					ID:      2,
					Columns: []ColumnDescriptor{{ID: 2}},
					PrimaryIndex: IndexDescriptor{
						ColumnIDs: ColumnIDs{2},
						ID:        2,
						ReferencedBy: []ForeignKeyReference{
							{
								Table: 1,
								Index: 1,
							},
						},
					},
				},
				expectedUpgraded: TableDescriptor{
					ID:      2,
					Columns: []ColumnDescriptor{{ID: 2}},
					PrimaryIndex: IndexDescriptor{
						ColumnIDs: ColumnIDs{2},
						ID:        2,
					},
					InboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   2,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
					},
				},
			},
		},
		2: {
			name: "self-reference-cycle",
			origin: oldFormatUpgradedPair{
				oldFormat: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ID:        1,
							ColumnIDs: ColumnIDs{1},
							ForeignKey: ForeignKeyReference{
								Table:           1,
								Index:           2,
								Name:            "foo",
								Validity:        ConstraintValidity_Validated,
								SharedPrefixLen: 1,
								OnDelete:        ForeignKeyReference_NO_ACTION,
								OnUpdate:        ForeignKeyReference_NO_ACTION,
								Match:           ForeignKeyReference_SIMPLE,
							},
							ReferencedBy: []ForeignKeyReference{
								{
									Table: 1,
									Index: 2,
								},
							},
						},
						{
							ID:        2,
							ColumnIDs: ColumnIDs{2},
							ForeignKey: ForeignKeyReference{
								Table:           1,
								Index:           1,
								Name:            "bar",
								Validity:        ConstraintValidity_Validating,
								SharedPrefixLen: 1,
								OnDelete:        ForeignKeyReference_CASCADE,
								OnUpdate:        ForeignKeyReference_CASCADE,
								Match:           ForeignKeyReference_PARTIAL,
							},
							ReferencedBy: []ForeignKeyReference{
								{
									Table: 1,
									Index: 1,
								},
							},
						},
					},
				},
				expectedUpgraded: TableDescriptor{
					ID:      1,
					Columns: []ColumnDescriptor{{ID: 1}, {ID: 2}},
					Indexes: []IndexDescriptor{
						{
							ID:        1,
							ColumnIDs: ColumnIDs{1},
						},
						{
							ID:        2,
							ColumnIDs: ColumnIDs{2},
						},
					},
					OutboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   1,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validated,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{2},
							ReferencedTableID:   1,
							ReferencedColumnIDs: ColumnIDs{1},
							Name:                "bar",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_CASCADE,
							OnUpdate:            ForeignKeyReference_CASCADE,
							Match:               ForeignKeyReference_PARTIAL,
						},
					},
					InboundFKs: []ForeignKeyConstraint{
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{2},
							ReferencedTableID:   1,
							ReferencedColumnIDs: ColumnIDs{1},
							Name:                "bar",
							Validity:            ConstraintValidity_Validating,
							OnDelete:            ForeignKeyReference_CASCADE,
							OnUpdate:            ForeignKeyReference_CASCADE,
							Match:               ForeignKeyReference_PARTIAL,
						},
						{
							OriginTableID:       1,
							OriginColumnIDs:     ColumnIDs{1},
							ReferencedTableID:   1,
							ReferencedColumnIDs: ColumnIDs{2},
							Name:                "foo",
							Validity:            ConstraintValidity_Validated,
							OnDelete:            ForeignKeyReference_NO_ACTION,
							OnUpdate:            ForeignKeyReference_NO_ACTION,
							Match:               ForeignKeyReference_SIMPLE,
						},
					},
				},
			},
			// NOTE: for this test case, we'll set this field to the same value as
			// the above, since it's a self-referencing table.
			referenced: oldFormatUpgradedPair{},
		},
	}

	// Set the self-referencing test case's referenced tables to the origin tables
	// to save on some typing.
	testCases[2].referenced = testCases[2].origin

	ctx := context.Background()
	for _, tc := range testCases {
		tc.origin.expectedUpgraded.Privileges = NewDefaultPrivilegeDescriptor()
		tc.origin.oldFormat.Privileges = NewDefaultPrivilegeDescriptor()
		tc.referenced.expectedUpgraded.Privileges = NewDefaultPrivilegeDescriptor()
		tc.referenced.oldFormat.Privileges = NewDefaultPrivilegeDescriptor()
		txn := fakeProtoGetter{protos: map[interface{}]protoutil.Message{
			string(MakeDescMetadataKey(tc.origin.oldFormat.ID)):     WrapDescriptor(&tc.origin.oldFormat),
			string(MakeDescMetadataKey(tc.referenced.oldFormat.ID)): WrapDescriptor(&tc.referenced.oldFormat),
		}}

		tables := []oldFormatUpgradedPair{tc.origin, tc.referenced}
		// For each test case, verify that both the origin and referenced tables
		// get upgraded to the expected state and then get downgraded back to the
		// original state.
		//
		// Additionally verify that downgrading on a cluster version that's
		// sufficiently new is a no-op.
		for i, pair := range tables {
			name := "origin"
			if i == 1 {
				name = "referenced"
			}
			t.Run(fmt.Sprintf("%s/%s", tc.name, name), func(t *testing.T) {
				upgraded := protoutil.Clone(&pair.oldFormat).(*TableDescriptor)
				wasUpgraded, err := upgraded.maybeUpgradeForeignKeyRepresentation(ctx, txn)
				if err != nil {
					t.Fatal(err)
				}
				if !wasUpgraded {
					t.Fatalf("expected proto to be upgraded")
				}

				wasUpgradedAgain, err := upgraded.maybeUpgradeForeignKeyRepresentation(ctx, txn)
				if wasUpgradedAgain {
					t.Fatalf("expected proto upgrade to be idempotent")
				}

				// The upgraded proto will also have a copy of the old foreign key
				// reference attached for each foreign key. Delete that for the purposes of
				// verifying equality.
				for i := range upgraded.OutboundFKs {
					pair.expectedUpgraded.OutboundFKs[i].LegacyUpgradedFromOriginReference = upgraded.OutboundFKs[i].LegacyUpgradedFromOriginReference
					pair.expectedUpgraded.OutboundFKs[i].LegacyOriginIndex = upgraded.OutboundFKs[i].LegacyOriginIndex
					pair.expectedUpgraded.OutboundFKs[i].LegacyReferencedIndex = upgraded.OutboundFKs[i].LegacyReferencedIndex
				}
				for i := range upgraded.InboundFKs {
					pair.expectedUpgraded.InboundFKs[i].LegacyUpgradedFromReferencedReference = upgraded.InboundFKs[i].LegacyUpgradedFromReferencedReference
					pair.expectedUpgraded.InboundFKs[i].LegacyOriginIndex = upgraded.InboundFKs[i].LegacyOriginIndex
					pair.expectedUpgraded.InboundFKs[i].LegacyReferencedIndex = upgraded.InboundFKs[i].LegacyReferencedIndex
				}
				if !reflect.DeepEqual(upgraded, &pair.expectedUpgraded) {
					t.Fatalf("upgrade didn't match original %s %s", proto.MarshalTextString(upgraded),
						proto.MarshalTextString(&pair.expectedUpgraded))
				}
				for i := range upgraded.OutboundFKs {
					pair.expectedUpgraded.OutboundFKs[i].LegacyUpgradedFromOriginReference = ForeignKeyReference{}
					pair.expectedUpgraded.OutboundFKs[i].LegacyOriginIndex = 0
					pair.expectedUpgraded.OutboundFKs[i].LegacyReferencedIndex = 0
				}
				for i := range upgraded.InboundFKs {
					pair.expectedUpgraded.InboundFKs[i].LegacyUpgradedFromReferencedReference = ForeignKeyReference{}
					pair.expectedUpgraded.InboundFKs[i].LegacyOriginIndex = 0
					pair.expectedUpgraded.InboundFKs[i].LegacyReferencedIndex = 0
				}

				wasDowngraded, downgraded, err := upgraded.maybeDowngradeForeignKeyRepresentation(ctx, txn, mixedVersionSettings)
				if err != nil {
					t.Fatal(err)
				}
				if !wasDowngraded {
					t.Fatalf("expected proto to be downgraded")
				}

				if !reflect.DeepEqual(downgraded, &pair.oldFormat) {
					t.Fatalf("downgrade didn't match original %s %s", proto.MarshalTextString(downgraded),
						proto.MarshalTextString(&pair.oldFormat))
				}

				// Check that the downgrade is idempotent as well. Downgrading the table
				// again shouldn't change it.

				wasDowngradedAgain, downgradedAgain, err := downgraded.maybeDowngradeForeignKeyRepresentation(ctx, txn, mixedVersionSettings)
				if err != nil {
					t.Fatal(err)
				}
				if wasDowngradedAgain {
					t.Fatalf("expected proto to not be downgraded a second time")
				}

				if !reflect.DeepEqual(downgradedAgain, downgraded) {
					t.Fatalf("downgrade wasn't idempotent %s %s", proto.MarshalTextString(downgradedAgain),
						proto.MarshalTextString(downgraded))
				}

				wasDowngraded, _, err = upgraded.maybeDowngradeForeignKeyRepresentation(ctx, txn, newVersionSettings)
				if err != nil {
					t.Fatal(err)
				}
				if wasDowngraded {
					t.Fatalf("expected proto not to be downgraded")
				}
			})
		}
	}
}
