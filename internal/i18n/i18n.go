// Package i18n picks the language of the CLI and the TUI: Russian for a
// terminal whose locale is a language of the CIS, English for everything else
// — the rule the web UI applies to the browser (web/src/lib/i18n/detect.ts).
// The server itself answers in English whatever the client speaks.
package i18n

import "strings"

// cis are the languages that get Russian, as in the web UI; Moldova's
// Romanian (ro_MD) is checked separately.
var cis = map[string]bool{"ru": true, "uk": true, "be": true, "kk": true, "ky": true, "uz": true, "tg": true, "tk": true, "hy": true, "az": true, "ka": true, "os": true, "tt": true, "ba": true}

var ru bool

// Detect sets the language from the environment once, before the commands
// are built: MP_LANG=ru|en wins, then the locale the way gettext reads it —
// LC_ALL, LC_MESSAGES, LANG (the first one set), with LANGUAGE in front of
// them unless the locale is C/POSIX.
func Detect(getenv func(string) string) { ru = FromEnv(getenv) == "ru" }

// FromEnv is the language ("ru" or "en") the environment asks for.
func FromEnv(getenv func(string) string) string {
	switch getenv("MP_LANG") {
	case "ru":
		return "ru"
	case "en":
		return "en"
	}
	locale := ""
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(name); v != "" {
			locale = v
			break
		}
	}
	if locale == "" || locale == "C" || locale == "POSIX" || strings.HasPrefix(locale, "C.") {
		return "en"
	}
	if first, _, _ := strings.Cut(getenv("LANGUAGE"), ":"); first != "" {
		locale = first
	}
	return fromLocale(locale)
}

// fromLocale maps a locale name such as ru_RU.UTF-8, kk_KZ or ro_MD@euro to
// the language shown.
func fromLocale(locale string) string {
	locale, _, _ = strings.Cut(locale, ".")
	locale, _, _ = strings.Cut(locale, "@")
	primary, region, _ := strings.Cut(strings.ToLower(strings.ReplaceAll(locale, "-", "_")), "_")
	if cis[primary] || (primary == "ro" && region == "md") {
		return "ru"
	}
	return "en"
}

// Set forces the language: "ru" or anything else for English (tests).
func Set(lang string) { ru = lang == "ru" }

// RU tells whether the texts are Russian.
func RU() bool { return ru }

// T is the text in the chosen language: T("Сайты", "Sites"). Both texts of a
// format string keep the same verbs in the same order (a test checks it).
func T(ruText, enText string) string {
	if ru {
		return ruText
	}
	return enText
}
