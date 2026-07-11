package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	restoreJournalName    = ".restore-journal.json"
	restoreJournalVersion = 1
)

var renameBackupTarget = os.Rename

type restoreJournal struct {
	Version     int                    `json:"version"`
	PID         int                    `json:"pid"`
	Hostname    string                 `json:"hostname"`
	StartedAt   string                 `json:"started_at"`
	AsideSuffix string                 `json:"aside_suffix"`
	Targets     []restoreJournalTarget `json:"targets"`
}

type restoreJournalTarget struct {
	Name    string `json:"name"`
	Staged  bool   `json:"staged"`
	Existed bool   `json:"existed"`
	Done    bool   `json:"done"`
	// RolledBack records durable rollback progress per target so an
	// interrupted rollback is RESUMABLE: a re-run skips already-reversed
	// targets instead of failing their now-satisfied invariants (Codex
	// review — a crash between an aside-restoring rename and journal
	// removal previously wedged recovery behind manual repair).
	RolledBack bool `json:"rolled_back,omitempty"`
}

func restoreJournalPath(home string) string { return filepath.Join(home, restoreJournalName) }

// writeRestoreJournal atomically publishes and durably syncs the recovery
// record. Promotion never performs its next rename until this succeeds.
func writeRestoreJournal(path string, journal restoreJournal) error {
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore journal: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, backupDirModePerm); err != nil {
		return fmt.Errorf("create restore journal dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".restore-journal-*")
	if err != nil {
		return fmt.Errorf("create restore journal temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(backupEntryMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod restore journal temp: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write restore journal temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync restore journal temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close restore journal temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish restore journal: %w", err)
	}
	return syncRestoreDir(dir)
}

func syncRestoreDir(dir string) error {
	//nolint:gosec // dir is the configured DevStrap home or its parent.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// promoteAllTargets is one journaled transaction across every restore target.
// Existing targets are all moved aside before any staged target is promoted;
// the shared asides remain until every target is durably marked done.
func promoteAllTargets(home, stage, journalPath string) error {
	now := time.Now()
	j := restoreJournal{
		Version:     restoreJournalVersion,
		PID:         os.Getpid(),
		Hostname:    hostname(),
		StartedAt:   now.UTC().Format(time.RFC3339Nano),
		AsideSuffix: ".bak-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(now.UnixNano(), 10),
	}
	for _, name := range backupTargets {
		_, stageErr := os.Stat(filepath.Join(stage, name))
		// Lstat preserves even a dangling live symlink as pre-restore state.
		_, dstErr := os.Lstat(filepath.Join(home, name))
		if stageErr != nil && !errors.Is(stageErr, os.ErrNotExist) {
			return fmt.Errorf("inspect staged %s: %w", name, stageErr)
		}
		if dstErr != nil && !errors.Is(dstErr, os.ErrNotExist) {
			return fmt.Errorf("inspect existing %s: %w", name, dstErr)
		}
		staged := stageErr == nil
		j.Targets = append(j.Targets, restoreJournalTarget{
			Name: name, Staged: staged, Existed: dstErr == nil, Done: !staged,
		})
	}
	if err := writeRestoreJournal(journalPath, j); err != nil {
		return err
	}

	fail := func(operationErr error) error {
		rolledBack, recoveryErr := recoverRestoreJournal(home)
		if recoveryErr != nil {
			return errors.Join(operationErr, fmt.Errorf("rollback incomplete; recovery journal retained: %w", recoveryErr))
		}
		// A failure syncing the last all-Done journal may still have published
		// that commit record. Successful recovery then rolled it forward and
		// durably swept the journal, so the restore completed despite the
		// transient write error.
		if !rolledBack {
			return nil
		}
		return operationErr
	}

	// A target absent from the archive is deliberately untouched.
	for _, target := range j.Targets {
		if !target.Staged || !target.Existed {
			continue
		}
		dst := filepath.Join(home, target.Name)
		if err := renameBackupTarget(dst, dst+j.AsideSuffix); err != nil {
			return fail(fmt.Errorf("move existing %s aside: %w", target.Name, err))
		}
	}

	for i := range j.Targets {
		target := &j.Targets[i]
		if !target.Staged {
			continue
		}
		dst := filepath.Join(home, target.Name)
		if err := renameBackupTarget(filepath.Join(stage, target.Name), dst); err != nil {
			return fail(fmt.Errorf("promote restored %s: %w", target.Name, err))
		}
		target.Done = true
		// If this write fails, the last durable journal still describes an
		// incomplete promotion, so recovery safely rolls the whole swap back.
		if err := writeRestoreJournal(journalPath, j); err != nil {
			return fail(fmt.Errorf("record promoted %s: %w", target.Name, err))
		}
	}

	if _, err := recoverRestoreJournal(home); err != nil {
		return err
	}
	return nil
}

// recoverRestoreJournal rolls forward only after every target is durably Done.
// Any earlier journal rolls back in reverse target order to the exact pre-
// restore state. The journal is removed only after every aside is swept.
func recoverRestoreJournal(home string) (rolledBack bool, err error) {
	journalPath := restoreJournalPath(home)
	//nolint:gosec // journalPath is fixed beneath the configured DevStrap home.
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var j restoreJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return false, fmt.Errorf("cannot parse restore journal %s; inspect it and recover the state directory manually (the journal was left untouched)", journalPath)
	}
	if !validRestoreJournal(j) {
		return false, fmt.Errorf("restore journal %s parsed but fails its safety invariants (version/pid/aside-suffix/target shape); inspect it and recover the state directory manually (the journal was left untouched)", journalPath)
	}

	allDone := true
	for _, target := range j.Targets {
		allDone = allDone && target.Done
	}
	// Validate the durable journal's filesystem invariants before mutating
	// anything. A promoted replacement of an existing target must still have
	// its old aside until commit; an all-Done commit must still have every
	// staged destination. Damage or manual deletion retains the journal and
	// fails closed instead of certifying a mixed/incomplete generation.
	for _, target := range j.Targets {
		if target.RolledBack {
			continue // this target's reversal is durably recorded; its aside is legitimately gone
		}
		dst := filepath.Join(home, target.Name)
		aside := dst + j.AsideSuffix
		if allDone && target.Staged {
			if _, statErr := os.Lstat(dst); statErr != nil {
				return false, fmt.Errorf("restore recovery invariant failed: promoted %s is missing; recovery journal retained: %w", target.Name, statErr)
			}
			continue
		}
		if !allDone && target.Existed && target.Staged {
			_, asideErr := os.Lstat(aside)
			if target.Done && asideErr != nil {
				return false, fmt.Errorf("restore recovery invariant failed: aside for promoted %s is missing; recovery journal retained: %w", target.Name, asideErr)
			}
			if !target.Done && errors.Is(asideErr, os.ErrNotExist) {
				if _, dstErr := os.Lstat(dst); dstErr != nil {
					return false, fmt.Errorf("restore recovery invariant failed: both %s and its aside are missing; recovery journal retained: %w", target.Name, dstErr)
				}
			} else if !target.Done && asideErr != nil {
				return false, fmt.Errorf("inspect aside %s: %w", target.Name, asideErr)
			}
		}
	}
	if !allDone {
		var recoveryErr error
		for i := len(j.Targets) - 1; i >= 0; i-- {
			target := j.Targets[i]
			if target.RolledBack {
				continue
			}
			dst := filepath.Join(home, target.Name)
			aside := dst + j.AsideSuffix
			reversed := false
			if target.Existed && target.Staged {
				if _, statErr := os.Lstat(aside); statErr == nil {
					if removeErr := os.RemoveAll(dst); removeErr != nil {
						recoveryErr = errors.Join(recoveryErr, fmt.Errorf("remove promoted %s: %w", target.Name, removeErr))
						continue
					}
					if renameErr := renameBackupTarget(aside, dst); renameErr != nil {
						recoveryErr = errors.Join(recoveryErr, fmt.Errorf("restore aside %s: %w", target.Name, renameErr))
					} else {
						reversed = true
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					recoveryErr = errors.Join(recoveryErr, fmt.Errorf("inspect aside %s: %w", target.Name, statErr))
				} else {
					reversed = true // never moved aside: nothing to reverse
				}
			} else if target.Staged && !target.Existed {
				// The rename may have succeeded just before a crash/journal-write
				// failure, even when Done is still false.
				if removeErr := os.RemoveAll(dst); removeErr != nil {
					recoveryErr = errors.Join(recoveryErr, fmt.Errorf("remove promoted %s: %w", target.Name, removeErr))
				} else {
					reversed = true
				}
			} else {
				reversed = true // unstaged target: never touched
			}
			// Persist rollback progress durably BEFORE moving on, so a crash
			// here resumes at the next target instead of failing the already-
			// reversed target's invariants.
			if reversed {
				j.Targets[i].RolledBack = true
				if writeErr := writeRestoreJournal(journalPath, j); writeErr != nil {
					recoveryErr = errors.Join(recoveryErr, fmt.Errorf("record rollback of %s: %w", target.Name, writeErr))
				}
			}
		}
		if recoveryErr != nil {
			return false, fmt.Errorf("restore rollback incomplete; recovery journal retained: %w", recoveryErr)
		}
		rolledBack = true
	} else {
		var cleanupErr error
		for _, target := range j.Targets {
			if target.Existed && target.Staged {
				if removeErr := os.RemoveAll(filepath.Join(home, target.Name) + j.AsideSuffix); removeErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove aside %s: %w", target.Name, removeErr))
				}
			}
		}
		if cleanupErr != nil {
			return false, fmt.Errorf("restore cleanup incomplete; recovery journal retained: %w", cleanupErr)
		}
	}

	// Make every rollback rename or forward-cleanup removal durable while the
	// recovery record still exists. Only then may the journal be removed.
	if err := syncRestoreDir(home); err != nil {
		return false, err
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// Persist the journal deletion separately. If this sync fails, recovered
	// targets are already durable and a surviving journal is safely idempotent.
	if err := syncRestoreDir(home); err != nil {
		return false, err
	}
	return rolledBack, nil
}

func validRestoreJournal(j restoreJournal) bool {
	if j.Version != restoreJournalVersion || j.PID <= 0 || len(j.Targets) != len(backupTargets) {
		return false
	}
	prefix := ".bak-" + strconv.Itoa(j.PID) + "-"
	if !strings.HasPrefix(j.AsideSuffix, prefix) || strings.ContainsAny(j.AsideSuffix, `/\\`) {
		return false
	}
	if _, err := strconv.ParseInt(strings.TrimPrefix(j.AsideSuffix, prefix), 10, 64); err != nil {
		return false
	}
	for i, target := range j.Targets {
		if target.Name != backupTargets[i] || (!target.Staged && !target.Done) {
			return false
		}
	}
	return true
}
