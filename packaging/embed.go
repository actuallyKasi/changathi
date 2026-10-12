// Package packaging embeds the launchd and systemd service templates that
// `changathi daemon install` fills in with this machine's paths.
package packaging

import _ "embed"

//go:embed dev.changathi.plist
var Launchd string

//go:embed changathi.service
var Systemd string
