package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
)

// displacedSuffix names the directory a volume's previous contents are moved
// to while the new generation is swapped in. On a successful restore it is
// removed; a crash mid-swap leaves it behind, which is what CleanDisplaced
// sweeps.
const displacedSuffix = ".displaced"

// stagingRoot is the directory all per-Project restore staging lives under.
const stagingRoot = "restore-staging"

// CleanDisplaced removes `*.displaced` directories left by a restore that died
// between setting a volume's previous data aside and swapping the new data in.
// Nothing references them and each holds a full copy of a volume, so they are
// pure waste.
//
// Note what this deliberately does *not* touch: staging directories. Those are
// not garbage — a surviving staging directory is the signal that tells the
// restore decision the volumes may be split across generations, and sweeping
// it would erase that and let a half-swapped Project be adopted as legitimate.
// Staging is owned entirely by RestoreGeneration, which clears it on entry and
// removes it only on success.
//
// Failure is not fatal: leftovers waste space but corrupt nothing.
func CleanDisplaced(dataRoot string) error {
	volumesPath, err := caravolume.VolumesRoot(dataRoot)
	if err != nil {
		return err
	}

	var firstErr error
	walkErr := filepath.WalkDir(volumesPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A tree that does not exist yet is not a problem.
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if !d.IsDir() || !strings.HasSuffix(d.Name(), displacedSuffix) {
			return nil
		}
		if rmErr := os.RemoveAll(path); rmErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("restore: remove displaced data %q: %w", path, rmErr)
		}
		return filepath.SkipDir
	})
	if walkErr != nil && !os.IsNotExist(walkErr) && firstErr == nil {
		firstErr = fmt.Errorf("restore: scan for displaced data: %w", walkErr)
	}
	return firstErr
}
