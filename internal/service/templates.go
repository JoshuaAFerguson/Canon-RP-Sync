package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// SystemdUnit renders the unit file for a Linux install.
//
// The unit waits for the network because discovery needs multicast, restarts on
// failure, and is deliberately unsandboxed apart from the storage it needs:
// people run this on home machines with photo directories in unpredictable
// places.
func SystemdUnit(cfg Config) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", cfg.Description)
	fmt.Fprintf(&b, "Documentation=https://github.com/JoshuaAFerguson/canon-rp-sync\n")
	fmt.Fprintf(&b, "After=network-online.target\n")
	fmt.Fprintf(&b, "Wants=network-online.target\n\n")

	fmt.Fprintf(&b, "[Service]\n")
	fmt.Fprintf(&b, "Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s %s\n", quoteArgs([]string{cfg.Executable}), quoteArgs(cfg.Args))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", cfg.WorkingDir)
	for _, k := range sortedEnv(cfg.Env) {
		fmt.Fprintf(&b, "Environment=%s=%s\n", k, cfg.Env[k])
	}
	if cfg.SystemWide && cfg.User != "" {
		fmt.Fprintf(&b, "User=%s\n", cfg.User)
	}
	fmt.Fprintf(&b, "Restart=always\n")
	fmt.Fprintf(&b, "RestartSec=5\n")
	// Imports can take a while; do not kill a transfer in progress.
	fmt.Fprintf(&b, "TimeoutStopSec=30\n\n")

	fmt.Fprintf(&b, "[Install]\n")
	if cfg.SystemWide {
		fmt.Fprintf(&b, "WantedBy=multi-user.target\n")
	} else {
		fmt.Fprintf(&b, "WantedBy=default.target\n")
	}
	return b.String()
}

// LaunchdPlist renders the property list for a macOS install.
func LaunchdPlist(cfg Config, label, logDir string) ([]byte, error) {
	args := append([]string{cfg.Executable}, cfg.Args...)

	var dict bytes.Buffer
	dict.WriteString(xml.Header)
	dict.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	dict.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeKeyString(&dict, "Label", label)

	dict.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		fmt.Fprintf(&dict, "\t\t<string>%s</string>\n", escapeXML(a))
	}
	dict.WriteString("\t</array>\n")

	writeKeyString(&dict, "WorkingDirectory", cfg.WorkingDir)
	dict.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	dict.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")

	if len(cfg.Env) > 0 {
		dict.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedEnv(cfg.Env) {
			fmt.Fprintf(&dict, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", escapeXML(k), escapeXML(cfg.Env[k]))
		}
		dict.WriteString("\t</dict>\n")
	}

	writeKeyString(&dict, "StandardOutPath", logDir+"/"+cfg.Name+".log")
	writeKeyString(&dict, "StandardErrorPath", logDir+"/"+cfg.Name+".err.log")

	dict.WriteString("</dict>\n</plist>\n")
	return dict.Bytes(), nil
}

func writeKeyString(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<string>%s</string>\n", escapeXML(key), escapeXML(value))
}

func escapeXML(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
