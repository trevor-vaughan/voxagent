// Package units lists the native units. It is the only package that imports
// them; internal/unit cannot (it takes the list as a parameter).
package units

import (
	"github.com/voxagent/voxagent/internal/unit"
	augeascore "github.com/voxagent/voxagent/internal/units/augeas_core"
	augeasproviderscore "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
	augeasprovidersssh "github.com/voxagent/voxagent/internal/units/augeasproviders_ssh"
	augeasproviderssysctl "github.com/voxagent/voxagent/internal/units/augeasproviders_sysctl"
	"github.com/voxagent/voxagent/internal/units/concat"
	croncore "github.com/voxagent/voxagent/internal/units/cron_core"
	"github.com/voxagent/voxagent/internal/units/exec"
	"github.com/voxagent/voxagent/internal/units/file"
	"github.com/voxagent/voxagent/internal/units/group"
	hostcore "github.com/voxagent/voxagent/internal/units/host_core"
	"github.com/voxagent/voxagent/internal/units/notify"
	pkg "github.com/voxagent/voxagent/internal/units/package"
	"github.com/voxagent/voxagent/internal/units/service"
	"github.com/voxagent/voxagent/internal/units/user"
)

// All returns every native unit. Each unit appends itself here, sorted by
// name.
func All() []unit.Unit {
	return []unit.Unit{augeascore.Unit, augeasproviderscore.Unit, augeasprovidersssh.Unit, augeasproviderssysctl.Unit, concat.Unit, croncore.Unit, exec.Unit, file.Unit, group.Unit, hostcore.Unit, notify.Unit, pkg.Unit, service.Unit, user.Unit}
}
