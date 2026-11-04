package ccbilling

import (
	"errors"
	"testing"
	"time"
)

func storedBlocks(t *testing.T, host *fakeHost) map[string]int64 {
	t.Helper()
	rows, err := host.db.Query(`SELECT scope, hour_at, reason, count FROM cc_block_counts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var scope, reason string
		var hour, count int64
		if err := rows.Scan(&scope, &hour, &reason, &count); err != nil {
			t.Fatal(err)
		}
		result[scope+"|"+timeAt(hour).Format("15:04")+"|"+reason] = count
	}
	return result
}

func TestBlockCountsAccumulatePerKeyHourAndReasonAndFlushOnce(t *testing.T) {
	manager, host := configuredManager(t)
	at := host.now
	for range 3 {
		manager.RecordBlock("remainder", RefusedWeekly, at.Add(10*time.Minute))
	}
	manager.RecordBlock("remainder", RefusedFiveHour, at.Add(20*time.Minute))
	manager.RecordBlock("remainder", RefusedWeekly, at.Add(70*time.Minute))
	manager.RecordBlock("", RefusedWeekly, at)
	if got := storedBlocks(t, host); len(got) != 0 {
		t.Fatalf("counts were written on the request path: %v", got)
	}
	if err := manager.FlushBlocks(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"remainder|10:00|weekly_reserve": 3, "remainder|10:00|five_hour_reserve": 1, "remainder|11:00|weekly_reserve": 1}
	got := storedBlocks(t, host)
	if len(got) != len(want) {
		t.Fatalf("counts = %v", got)
	}
	for key, count := range want {
		if got[key] != count {
			t.Fatalf("counts = %v, want %v", got, want)
		}
	}
	// A second flush adds nothing; later refusals add to the stored hour.
	if err := manager.FlushBlocks(); err != nil {
		t.Fatal(err)
	}
	manager.RecordBlock("remainder", RefusedWeekly, at.Add(30*time.Minute))
	// A usage-triggered refresh within the interval still saves the counts.
	manager.RefreshInterval = time.Hour
	manager.MaybeRefresh()
	if got := storedBlocks(t, host); got["remainder|10:00|weekly_reserve"] != 4 || len(got) != 3 {
		t.Fatalf("counts = %v", got)
	}

	// A failed save keeps the counts for the next one.
	manager.RecordBlock("remainder", RefusedWeekly, at.Add(40*time.Minute))
	host.dbErr = errors.New("database unavailable")
	if err := manager.FlushBlocks(); err == nil {
		t.Fatal("flush succeeded without a database")
	}
	host.dbErr = nil
	if err := manager.FlushBlocks(); err != nil {
		t.Fatal(err)
	}
	if got := storedBlocks(t, host); got["remainder|10:00|weekly_reserve"] != 5 {
		t.Fatalf("counts after a failed save = %v", got)
	}
}
