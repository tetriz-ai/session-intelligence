// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import "strings"

// This mirrors the desktop app's shared map in
// the desktop client's language map. Both exist
// because the desktop app derives languages at extraction time (the source of
// truth) and this backend copy is a fallback for older desktop builds and for
// IDEs whose extractor does not derive languages (Codex/Copilot/Antigravity).
// Keep the two in sync when adding a language.

// extToLanguage keys on the bare, lowercased extension (no leading dot).
var extToLanguage = map[string]string{
	"ts": "TypeScript", "tsx": "TypeScript",
	"js": "JavaScript", "jsx": "JavaScript", "mjs": "JavaScript", "cjs": "JavaScript",
	"py": "Python", "go": "Go", "rs": "Rust", "java": "Java", "kt": "Kotlin", "kts": "Kotlin",
	"swift": "Swift", "cpp": "C++", "cc": "C++", "cxx": "C++", "hpp": "C++",
	"c": "C", "h": "C", "cs": "C#", "rb": "Ruby", "php": "PHP", "scala": "Scala",
	"sh": "Shell", "bash": "Shell", "zsh": "Shell", "fish": "Shell",
	"sql": "SQL", "html": "HTML", "css": "CSS", "scss": "CSS", "sass": "CSS", "less": "CSS",
	"json": "JSON", "yaml": "YAML", "yml": "YAML", "toml": "TOML", "md": "Markdown", "mdx": "Markdown",
	"tf": "Terraform", "tfvars": "Terraform", "hcl": "HCL",
	"proto": "Protobuf", "graphql": "GraphQL", "gql": "GraphQL",
	"groovy": "Groovy", "gradle": "Groovy",
}

// basenameToLanguage keys on the exact file basename, for extensionless or
// convention-named files. Basename wins over extension.
var basenameToLanguage = map[string]string{
	"Makefile": "Makefile", "GNUmakefile": "Makefile", "makefile": "Makefile",
	"Dockerfile": "Dockerfile", "Containerfile": "Dockerfile",
	"Jenkinsfile": "Groovy",
	"Rakefile":    "Ruby", "Gemfile": "Ruby", "Guardfile": "Ruby",
	"Vagrantfile": "Ruby", "Brewfile": "Ruby",
	"Justfile": "Just", "justfile": "Just",
}

// languageForPath returns the language for a single file path, or "" when the
// basename/extension is unmapped. Basename is tried before extension; a
// leading-dot file (".gitignore") has no extension by this rule (matches the TS
// mirror, which also treats a dot at index 0 as "no extension").
func languageForPath(filePath string) string {
	base := filePath
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if lang, ok := basenameToLanguage[base]; ok {
		return lang
	}
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return ""
	}
	return extToLanguage[strings.ToLower(base[dot+1:])]
}

// deriveLanguagesFromFiles counts language occurrences across file paths,
// returning a language → file-count map (empty when nothing maps). Shape
// matches SessionMeta.languages (map<string, int32>).
func deriveLanguagesFromFiles(files []string) map[string]int32 {
	counts := make(map[string]int32)
	for _, f := range files {
		if lang := languageForPath(f); lang != "" {
			counts[lang]++
		}
	}
	return counts
}

// languageNames flattens a language→count map into the name slice that
// UsageMetadata.Languages (repeated string) holds; order is unspecified.
func languageNames(counts map[string]int32) []string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	return names
}
