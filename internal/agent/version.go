package agent

import "regexp"

// Version é injetada pelo build.sh via ldflags
// "-X localissimo-print-agent/internal/agent.Version=X.Y.Z".
// Build de dev fica "0.0.0-dev" (suja): não casa o formato de release e o
// auto-update é ignorado com motivo no log — build de dev nunca troca o
// próprio binário (epic §7.4, "build sujo de dev ignora e loga o motivo").
var Version = "0.0.0-dev"

// Mesma forma do epic §7.4: aceita prefixo "v" opcional. O catálogo do Rails
// (PrinterAgentRelease) exige X.Y.Z sem prefixo — o build.sh gera sem "v".
var versionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// IsValidReleaseVersion: a string casa X.Y.Z (com prefixo "v" opcional)?
// Builds de dev ("0.0.0-dev") não casam — e o auto-update os ignora.
func IsValidReleaseVersion(s string) bool {
	return versionRe.MatchString(s)
}
