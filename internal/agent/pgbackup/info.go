// Package pgbackup takes pgBackRest backups of PostgresDifferential databases
// while they run.
package pgbackup

import (
	"encoding/json"
	"fmt"
	"time"
)

// Backup types the runner takes.
const (
	TypeFull = "full"
	TypeDiff = "diff"
)

// backupSet is one backup in a stanza, as `pgbackrest info` lists it.
type backupSet struct {
	Label string `json:"label"`
	Type  string `json:"type"`
	// Timestamp holds Unix seconds.
	Timestamp struct {
		Start int64 `json:"start"`
		Stop  int64 `json:"stop"`
	} `json:"timestamp"`
	Archive struct {
		Start string `json:"start"`
		Stop  string `json:"stop"`
	} `json:"archive"`
}

// started is when the backup started.
func (b backupSet) started() time.Time { return time.Unix(b.Timestamp.Start, 0).UTC() }

// timeline is the PostgreSQL timeline the backup was taken on: the first eight
// hex digits of its first WAL segment name.
func (b backupSet) timeline() string {
	if len(b.Archive.Start) < 8 {
		return ""
	}
	return b.Archive.Start[:8]
}

type stanzaInfo struct {
	Name   string      `json:"name"`
	Backup []backupSet `json:"backup"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

// parseInfo reads `pgbackrest --stanza=S --output=json info` for one stanza.
// Backups are listed oldest first, which is pgBackRest's order.
func parseInfo(out []byte, stanza string) ([]backupSet, error) {
	var stanzas []stanzaInfo
	if err := json.Unmarshal(out, &stanzas); err != nil {
		return nil, fmt.Errorf("pgbackup: parse info: %w", err)
	}
	for _, s := range stanzas {
		if s.Name != stanza {
			continue
		}
		// 0 is ok and 2 is "no valid backups"; anything else means the stanza
		// cannot be read, and choosing a type from it would be a guess.
		if s.Status.Code != 0 && s.Status.Code != 2 {
			return nil, fmt.Errorf("pgbackup: stanza %s: %s (status %d)", stanza, s.Status.Message, s.Status.Code)
		}
		return s.Backup, nil
	}
	return nil, fmt.Errorf("pgbackup: info does not list stanza %s", stanza)
}

// newestFull is the most recent full backup, or false when there is none.
func newestFull(sets []backupSet) (backupSet, bool) {
	for i := len(sets) - 1; i >= 0; i-- {
		if sets[i].Type == TypeFull {
			return sets[i], true
		}
	}
	return backupSet{}, false
}

// Selection is the backup type chosen for one run and why.
type Selection struct {
	Type   string
	Reason string
}

// chooseType applies the selection rules in order. Every rule that can fire
// for a full is checked before differential is allowed, so the cheaper type
// is chosen only when nothing argues against it.
func chooseType(sets []backupSet, timeline string, continuityKnown bool, now time.Time, fullInterval time.Duration) Selection {
	full, ok := newestFull(sets)
	switch {
	case !ok:
		return Selection{TypeFull, "the stanza has no full backup"}
	case now.Sub(full.started()) >= fullInterval:
		return Selection{TypeFull, "the newest full backup is older than the full interval"}
	case full.timeline() != timeline:
		return Selection{TypeFull, "the database is on timeline " + timeline + ", the newest full backup on " + full.timeline()}
	case !continuityKnown:
		return Selection{TypeFull, "WAL archiving may not have been continuous since the newest full backup"}
	default:
		return Selection{TypeDiff, "a recent full backup on the current timeline"}
	}
}
