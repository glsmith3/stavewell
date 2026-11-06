package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/record"
)

// BenchmarkVerifyChain measures chain verification of 10,000 records, the
// most one request checks, with data of three sizes. The records are
// inserted directly, in one transaction, so that setting up doesn't take
// 10,000 separate appends.
//
// Run it with: go test -run '^$' -bench VerifyChain ./internal/store/
func BenchmarkVerifyChain(b *testing.B) {
	const n = 10_000
	for _, size := range []int{100, 1 << 10, record.MaxDataBytes} {
		b.Run(fmt.Sprintf("%d_bytes", size), func(b *testing.B) {
			ctx := context.Background()
			s, err := Open(ctx, b.TempDir()+"/bench.db")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			org, err := s.CreateOrganization(ctx, "Bench", testNow)
			if err != nil {
				b.Fatal(err)
			}
			var orgRow int64
			if err := s.read.QueryRow("SELECT id FROM organizations WHERE public_id = ?", org.ID).Scan(&orgRow); err != nil {
				b.Fatal(err)
			}
			if _, err := s.CreateLedger(ctx, orgRow, "orders", testNow); err != nil {
				b.Fatal(err)
			}

			tx, err := s.write.Begin()
			if err != nil {
				b.Fatal(err)
			}
			var lid int64
			if err := tx.QueryRow("SELECT id FROM ledgers WHERE organization_id = ?", orgRow).Scan(&lid); err != nil {
				b.Fatal(err)
			}
			previous := record.ZeroHash
			for i := 1; i <= n; i++ {
				prefix := fmt.Sprintf(`{"i":%d,"p":"`, i)
				d, err := record.ParseData([]byte(prefix + strings.Repeat("x", size-len(prefix)-2) + `"}`))
				if err != nil {
					b.Fatal(err)
				}
				created := FormatTime(testNow.Add(time.Duration(i) * time.Millisecond))
				h, err := record.Hash(record.Fields{Ledger: "orders", Sequence: int64(i), CreatedAt: created, DataHash: d.Hash(), PreviousHash: previous})
				if err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?)`,
					lid, i, created, d.Canonical(), d.Hash(), previous, h); err != nil {
					b.Fatal(err)
				}
				previous = h
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			for b.Loop() {
				res, err := s.VerifyChain(ctx, orgRow, "orders", 1, n)
				if err != nil {
					b.Fatal(err)
				}
				if !res.Intact || res.Last != n {
					b.Fatalf("result = %+v", res)
				}
			}
		})
	}
}
