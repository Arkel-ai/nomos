// Package casbinmodel embeds the Casbin RBAC-with-domains model so it
// loads reliably regardless of the process working directory (the file
// lives next to this Go file). Edit model.conf, not a Go string.
package casbinmodel

import _ "embed"

//go:embed model.conf
var Model string
