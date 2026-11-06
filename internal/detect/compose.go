package detect

import (
	"path"
	"strings"
)

type composeFile struct {
	file     string
	services []ComposeService
}

// Compose files in order of precedence (the Compose specification's).
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

func (s *scanner) compose(dir string) (*composeFile, error) {
	name, err := s.firstFile(dir, composeNames...)
	if err != nil || name == "" {
		return nil, err
	}
	text, err := s.text(path.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return &composeFile{file: name, services: parseComposeServices(text)}, nil
}

// parseComposeServices reads the service names, images and build keys of a
// compose file. It understands the block YAML that compose files use; flow
// mappings and anchors beyond plain keys are not needed for detection.
func parseComposeServices(text string) []ComposeService {
	var services []ComposeService
	inServices := false
	serviceIndent, fieldIndent := -1, -1
	var current *ComposeService
	flush := func() {
		if current != nil {
			services = append(services, *current)
			current = nil
		}
	}
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\t", "  "), "\n") {
		line := stripYAMLComment(raw)
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, isKey := yamlKey(strings.TrimSpace(line))
		if indent == 0 {
			flush()
			inServices = isKey && key == "services"
			serviceIndent, fieldIndent = -1, -1
			continue
		}
		if !inServices {
			continue
		}
		if serviceIndent < 0 {
			serviceIndent = indent
		}
		if indent == serviceIndent {
			flush()
			if isKey && !strings.HasPrefix(key, "x-") {
				current = &ComposeService{Name: key}
			}
			fieldIndent = -1
			continue
		}
		if current == nil || indent < serviceIndent {
			continue
		}
		if fieldIndent < 0 {
			fieldIndent = indent
		}
		if indent != fieldIndent || !isKey {
			continue
		}
		switch key {
		case "image":
			current.Image = unquote(value)
		case "build":
			current.Build = true
		}
	}
	flush()
	for i := range services {
		services[i].Role = serviceRole(services[i])
	}
	return services
}

func stripYAMLComment(line string) string {
	quote := rune(0)
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' '):
			return strings.TrimRight(line[:i], " ")
		}
	}
	return strings.TrimRight(line, " \r")
}

func yamlKey(line string) (string, string, bool) {
	if strings.HasPrefix(line, "- ") || line == "-" {
		return "", "", false
	}
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	key = unquote(strings.TrimSpace(key))
	if key == "" || strings.ContainsAny(key, " {}[]") {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// imageName returns an image reference's repository name without registry,
// namespace, tag or digest: "ghcr.io/acme/synapse:v1" is "synapse".
func imageName(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	image, _, _ = strings.Cut(image, ":")
	return strings.ToLower(image)
}

var (
	postgresImages    = map[string]bool{"postgres": true, "postgis": true, "pgvector": true, "timescaledb": true}
	redisImages       = map[string]bool{"redis": true, "valkey": true, "keydb": true, "redis-stack-server": true}
	clickhouseImages  = map[string]bool{"clickhouse-server": true, "clickhouse": true}
	developmentImages = map[string]bool{"mailpit": true, "mailhog": true, "mailcatcher": true, "maildev": true, "adminer": true, "pgadmin4": true, "redis-commander": true, "redisinsight": true}
)

func isRedisImage(image string) bool { return redisImages[imageName(image)] }

func isClickHouseImage(image string) bool { return clickhouseImages[imageName(image)] }

func serviceRole(s ComposeService) string {
	name := imageName(s.Image)
	switch {
	case s.Build:
		return "source"
	case postgresImages[name] || redisImages[name] || clickhouseImages[name]:
		return "data"
	case developmentImages[name]:
		return "development"
	case s.Image == "":
		// Neither an image nor a build: an extension or incomplete service.
		return "development"
	}
	return "image"
}
