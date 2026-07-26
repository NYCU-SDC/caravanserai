//go:build e2e

package backup

import (
	"NYCU-SDC/caravanserai/test/integration/testhelper"
)

// startMinio is a thin alias so the test package reads naturally.
var startMinio = testhelper.StartMinio
