package agent

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Reading a Rails config/database.yml as data.
//
// The agent never runs Ruby. ERB output tags of exactly these forms are read
// textually, with their variable name and literal default:
//
//	<%= ENV["X"] %>   <%= ENV.fetch("X") %>   <%= ENV.fetch("X", "default") %>
//	<%= ENV.fetch("X") { default } %>
//
// Any other output tag is opaque Ruby: allowed in a setting that does not
// select the database (pool, timeouts), refused in one that does. Control
// tags (<% if %>) can change the file's structure and are refused wherever
// they are. The YAML is the subset Rails applications write: nested
// mappings, anchors, aliases, merge keys, quoted and plain scalars and simple
// lists. Anything else fails closed.

// railsDatabaseIdentityFields are the settings of one database entry that
// select which database it connects to.
var railsDatabaseIdentityFields = []string{"url", "adapter", "host", "hostaddr", "port", "database", "dbname", "username", "user", "socket", "service"}

// railsConfigValue is one setting, rendered canonically.
type railsConfigValue struct {
	Text string // with ERB tags rendered as their canonical expression
	Env  string // the variable when the value is exactly one ENV tag
	Ruby bool   // contains an output tag the agent cannot read
}

// railsDatabaseConfig is the production section of a database.yml.
type railsDatabaseConfig struct {
	Tiered  bool                                   // production holds named databases (primary:, queue:)
	Entries map[string]map[string]railsConfigValue // database name -> flattened setting -> value
	Code    bool                                   // has ERB control code or Ruby in a key
}

// railsConfigChange is why a changed database.yml cannot be re-approved.
type railsConfigChange struct {
	Message string // a sentence starting with "database.yml"
	Detail  string
}

func (c *railsConfigChange) Error() string {
	if c.Detail != "" {
		return c.Message + " (" + c.Detail + ")"
	}
	return c.Message
}

const railsConfigRubyMessage = "database.yml computes connection settings with Ruby"

type erbTag struct {
	Env     string // variable, for a readable ENV form
	Default string // canonical literal default, or ""
	Form    string // canonical expression
	Ruby    bool
}

var (
	erbEnvIndex   = regexp.MustCompile(`^ENV\[\s*(?:"([A-Za-z_][A-Za-z0-9_]*)"|'([A-Za-z_][A-Za-z0-9_]*)')\s*\]$`)
	erbEnvFetch   = regexp.MustCompile(`^ENV\.fetch\(\s*(?:"([A-Za-z_][A-Za-z0-9_]*)"|'([A-Za-z_][A-Za-z0-9_]*)')\s*(?:,\s*(.*?)\s*)?\)(?:\s*\{\s*(.+?)\s*\}|\s+do\s+(.+?)\s+end)?$`)
	erbLiteral    = regexp.MustCompile(`^(?:"[^"\\#]*"|'[^'\\]*'|-?[0-9]+|nil|true|false)$`)
	yamlAliasName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

const erbOpen, erbClose = '', ''

// readERBTag classifies the expression of one <%= %> tag.
func readERBTag(expr string) erbTag {
	expr = strings.TrimSpace(expr)
	if m := erbEnvIndex.FindStringSubmatch(expr); m != nil {
		name := m[1] + m[2]
		return erbTag{Env: name, Form: `ENV["` + name + `"]`}
	}
	if m := erbEnvFetch.FindStringSubmatch(expr); m != nil {
		name := m[1] + m[2]
		defaults := 0
		literal := ""
		for _, d := range m[3:] {
			if d != "" {
				defaults++
				literal = d
			}
		}
		if defaults > 1 || defaults == 1 && !erbLiteral.MatchString(literal) {
			return erbTag{Ruby: true, Form: "<%= " + expr + " %>"}
		}
		if literal == "" {
			return erbTag{Env: name, Form: `ENV.fetch("` + name + `")`}
		}
		return erbTag{Env: name, Default: literal, Form: `ENV.fetch("` + name + `", ` + literal + `)`}
	}
	return erbTag{Ruby: true, Form: "<%= " + expr + " %>"}
}

// expandRailsERB replaces each ERB tag with a placeholder naming its entry in
// tags. code reports control tags, which are removed.
func expandRailsERB(src string) (text string, tags []erbTag, code bool, err error) {
	var out strings.Builder
	for {
		start := strings.Index(src, "<%")
		if start < 0 {
			out.WriteString(src)
			break
		}
		out.WriteString(src[:start])
		rest := src[start+2:]
		end := strings.Index(rest, "%>")
		if end < 0 {
			return "", nil, false, errors.New("an ERB tag is not closed")
		}
		body := rest[:end]
		src = rest[end+2:]
		switch {
		case strings.HasPrefix(body, "#"):
			// comment
		case strings.HasPrefix(body, "%"):
			code = true // a literal <%% is unusual enough to review
		case strings.HasPrefix(body, "="), strings.HasPrefix(body, "-="):
			expr := strings.TrimPrefix(strings.TrimPrefix(body, "-"), "=")
			expr = strings.TrimSuffix(expr, "-")
			if strings.ContainsAny(expr, "\n") {
				expr = strings.Join(strings.Fields(expr), " ")
			}
			out.WriteRune(erbOpen)
			out.WriteString(strconv.Itoa(len(tags)))
			out.WriteRune(erbClose)
			tags = append(tags, readERBTag(expr))
		default:
			code = true
		}
	}
	return out.String(), tags, code, nil
}

// railsConfigValueOf renders a scalar holding placeholders.
func railsConfigValueOf(raw string, tags []erbTag) railsConfigValue {
	var v railsConfigValue
	var text strings.Builder
	count, onlyTag := 0, -1
	for {
		start := strings.IndexRune(raw, erbOpen)
		if start < 0 {
			text.WriteString(raw)
			break
		}
		text.WriteString(raw[:start])
		raw = raw[start+utf8.RuneLen(erbOpen):]
		end := strings.IndexRune(raw, erbClose)
		n, err := strconv.Atoi(raw[:max(end, 0)])
		if end < 0 || err != nil || n < 0 || n >= len(tags) {
			v.Ruby = true // a placeholder the YAML reader split; fail closed
			continue
		}
		raw = raw[end+utf8.RuneLen(erbClose):]
		text.WriteString(tags[n].Form)
		v.Ruby = v.Ruby || tags[n].Ruby
		count++
		onlyTag = n
	}
	v.Text = text.String()
	if count == 1 && !v.Ruby && strings.TrimSpace(v.Text) == tags[onlyTag].Form {
		v.Env = tags[onlyTag].Env
		v.Text = tags[onlyTag].Form
	}
	return v
}

// parseRailsDatabaseConfig reads the production section of a database.yml.
func parseRailsDatabaseConfig(data []byte) (railsDatabaseConfig, error) {
	text, tags, code, err := expandRailsERB(string(data))
	if err != nil {
		return railsDatabaseConfig{}, fmt.Errorf("database.yml could not be read: %v", err)
	}
	root, err := parseYAMLSubset(text)
	if err != nil {
		return railsDatabaseConfig{}, fmt.Errorf("database.yml could not be read: %v", err)
	}
	cfg := railsDatabaseConfig{Code: code, Entries: map[string]map[string]railsConfigValue{}}
	if root.kind != yamlMap {
		return cfg, errors.New("database.yml could not be read: it is not a mapping of environments")
	}
	production, ok := root.items["production"]
	if !ok || production.kind != yamlMap {
		return cfg, errors.New("database.yml has no production section")
	}
	cfg.Tiered = len(production.keys) > 0
	for _, key := range production.keys {
		if production.items[key].kind != yamlMap {
			cfg.Tiered = false
		}
	}
	entries := map[string]*yamlNode{"primary": production}
	if cfg.Tiered {
		entries = production.items
	}
	for name, node := range entries {
		if strings.ContainsRune(name, erbOpen) {
			cfg.Code = true
		}
		flat := map[string]railsConfigValue{}
		flattenRailsConfig(node, "", tags, flat, &cfg.Code)
		cfg.Entries[name] = flat
	}
	return cfg, nil
}

func flattenRailsConfig(node *yamlNode, prefix string, tags []erbTag, out map[string]railsConfigValue, code *bool) {
	for _, key := range node.keys {
		if strings.ContainsRune(key, erbOpen) {
			*code = true
		}
		child := node.items[key]
		switch child.kind {
		case yamlMap:
			flattenRailsConfig(child, prefix+key+".", tags, out, code)
			if len(child.keys) == 0 {
				out[prefix+key] = railsConfigValue{Text: "{}"}
			}
		case yamlSeq:
			parts := make([]string, 0, len(child.seq))
			ruby := false
			for _, item := range child.seq {
				v := railsConfigValueOf(item.render(), tags)
				parts = append(parts, v.Text)
				ruby = ruby || v.Ruby
			}
			out[prefix+key] = railsConfigValue{Text: "[" + strings.Join(parts, ", ") + "]", Ruby: ruby}
		default:
			out[prefix+key] = railsConfigValueOf(child.value, tags)
		}
	}
}

// compareRailsDatabaseConfig compares an approved database.yml with a
// changed one. It returns the settings that changed when no database
// identity did, and a *railsConfigChange naming what changed otherwise.
func compareRailsDatabaseConfig(approved, current []byte) ([]string, error) {
	after, err := parseRailsDatabaseConfig(current)
	if err != nil {
		return nil, &railsConfigChange{Message: err.Error()}
	}
	if after.Code {
		return nil, &railsConfigChange{Message: railsConfigRubyMessage, Detail: "ERB control code or a computed key"}
	}
	names := railsMapKeys(after.Entries)
	for _, name := range names {
		for _, field := range railsDatabaseIdentityFields {
			if v, ok := after.Entries[name][field]; ok && v.Ruby {
				return nil, &railsConfigChange{Message: railsConfigRubyMessage, Detail: railsDatabaseLabel(after, name) + "'s " + field}
			}
		}
	}
	before, err := parseRailsDatabaseConfig(approved)
	if err != nil {
		return nil, &railsConfigChange{Message: "the approved database.yml cannot be compared", Detail: err.Error()}
	}
	var changes []string
	if before.Tiered != after.Tiered {
		changes = append(changes, "database.yml changed between one production database and named databases")
	}
	for _, name := range railsMapKeys(before.Entries) {
		if _, ok := after.Entries[name]; !ok {
			changes = append(changes, "database.yml no longer defines "+railsDatabaseLabel(before, name))
		}
	}
	for _, name := range names {
		if _, ok := before.Entries[name]; !ok {
			changes = append(changes, "database.yml now defines "+railsDatabaseLabel(after, name))
		}
	}
	for _, name := range names {
		old, ok := before.Entries[name]
		if !ok {
			continue
		}
		now := after.Entries[name]
		label := railsDatabaseLabel(after, name)
		sqlite := strings.Contains(old["adapter"].Text, "sqlite") || strings.Contains(now["adapter"].Text, "sqlite")
		for _, field := range railsDatabaseIdentityFields {
			a, b := old[field], now[field]
			if a.Text == b.Text {
				continue
			}
			switch {
			case field == "url" && a.Env != "" && b.Env != "" && a.Env != b.Env:
				changes = append(changes, fmt.Sprintf("database.yml now reads %s from %s instead of %s", label, b.Env, a.Env))
			case field == "database" && sqlite:
				changes = append(changes, fmt.Sprintf("database.yml changed %s's SQLite path from %s to %s", label, describeRailsValue(field, a), describeRailsValue(field, b)))
			default:
				changes = append(changes, fmt.Sprintf("database.yml changed %s's %s from %s to %s", label, field, describeRailsValue(field, a), describeRailsValue(field, b)))
			}
		}
	}
	if len(changes) > 0 {
		message := changes[0]
		for _, c := range changes[1:] {
			message += "; " + strings.TrimPrefix(c, "database.yml ")
		}
		return nil, &railsConfigChange{Message: message}
	}
	changed := []string{}
	for _, name := range names {
		old, now := before.Entries[name], after.Entries[name]
		keys := railsMapKeys(old)
		for key := range now {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			if old[key] != now[key] {
				if after.Tiered {
					changed = append(changed, name+"."+key)
				} else {
					changed = append(changed, key)
				}
			}
		}
	}
	if len(changed) == 0 {
		changed = []string{"comments, formatting or other environments"}
	}
	return changed, nil
}

func railsMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func railsDatabaseLabel(cfg railsDatabaseConfig, name string) string {
	if !cfg.Tiered {
		return "the production database"
	}
	return "the " + name + " database"
}

// describeRailsValue names a setting's value in a message without its
// password.
func describeRailsValue(field string, v railsConfigValue) string {
	text := v.Text
	switch {
	case text == "":
		return "unset"
	case v.Env != "":
		return text
	case field == "url":
		if u, err := url.Parse(text); err == nil && u.User != nil {
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(u.User.Username(), "xxxxx")
			}
			text = u.String()
		} else if err != nil {
			text = "a literal URL"
		}
	}
	if len(text) > 120 {
		text = text[:117] + "..."
	}
	return text
}

// A small YAML reader for database.yml.

type yamlKind int

const (
	yamlScalar yamlKind = iota
	yamlMap
	yamlSeq
)

type yamlNode struct {
	kind  yamlKind
	value string
	keys  []string
	items map[string]*yamlNode
	seq   []*yamlNode
}

func (n *yamlNode) render() string {
	switch n.kind {
	case yamlMap:
		parts := make([]string, 0, len(n.keys))
		for _, k := range n.keys {
			parts = append(parts, k+": "+n.items[k].render())
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case yamlSeq:
		parts := make([]string, 0, len(n.seq))
		for _, item := range n.seq {
			parts = append(parts, item.render())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return n.value
}

type yamlLine struct {
	indent int
	text   string
	no     int
}

type yamlParser struct {
	lines   []yamlLine
	i       int
	anchors map[string]*yamlNode
}

func parseYAMLSubset(src string) (*yamlNode, error) {
	p := &yamlParser{anchors: map[string]*yamlNode{}}
	for n, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(stripYAMLComment(strings.TrimRight(line, "\r")), " \t")
		body := strings.TrimLeft(line, " ")
		if body == "" {
			continue
		}
		if strings.HasPrefix(body, "\t") {
			return nil, fmt.Errorf("line %d is indented with a tab", n+1)
		}
		indent := len(line) - len(body)
		if indent == 0 && (body == "---" || body == "...") {
			if len(p.lines) > 0 {
				return nil, fmt.Errorf("line %d starts another YAML document", n+1)
			}
			continue
		}
		if indent == 0 && strings.HasPrefix(body, "%") {
			return nil, fmt.Errorf("line %d has a YAML directive", n+1)
		}
		p.lines = append(p.lines, yamlLine{indent: indent, text: body, no: n + 1})
	}
	if len(p.lines) == 0 {
		return &yamlNode{kind: yamlMap, items: map[string]*yamlNode{}}, nil
	}
	node, err := p.block(p.lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		return nil, fmt.Errorf("line %d has unexpected indentation", p.lines[p.i].no)
	}
	return node, nil
}

// stripYAMLComment removes a # comment outside quoted scalars.
func stripYAMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"':
			if c == '\\' {
				i++
			} else if c == '"' {
				quote = 0
			}
		case quote == '\'':
			if c == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		case (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" \t:-[{,", line[i-1]) >= 0):
			quote = c
		}
	}
	return line
}

func yamlSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *yamlParser) block(indent int) (*yamlNode, error) {
	if yamlSeqItem(p.lines[p.i].text) {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *yamlParser) mapping(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlMap, items: map[string]*yamlNode{}}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d has unexpected indentation", l.no)
		}
		if yamlSeqItem(l.text) {
			return nil, fmt.Errorf("line %d is a list item where a setting was expected", l.no)
		}
		key, rest, err := splitYAMLKey(l.text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", l.no, err)
		}
		p.i++
		value, err := p.value(rest, indent, l.no)
		if err != nil {
			return nil, err
		}
		if key == "<<" {
			// Psych merges in order: the merged settings replace earlier
			// ones, later settings replace merged ones.
			if value.kind != yamlMap {
				return nil, fmt.Errorf("line %d merges something other than one mapping", l.no)
			}
			for _, k := range value.keys {
				node.set(k, value.items[k])
			}
			continue
		}
		node.set(key, value)
	}
	return node, nil
}

func (n *yamlNode) set(key string, value *yamlNode) {
	if _, ok := n.items[key]; !ok {
		n.keys = append(n.keys, key)
	}
	n.items[key] = value
}

func (p *yamlParser) sequence(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlSeq}
	for p.i < len(p.lines) && p.lines[p.i].indent == indent && yamlSeqItem(p.lines[p.i].text) {
		l := p.lines[p.i]
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		if _, _, err := splitYAMLKey(rest); err == nil && rest != "" && rest[0] != '"' && rest[0] != '\'' {
			return nil, fmt.Errorf("line %d has a mapping inside a list", l.no)
		}
		p.i++
		item, err := p.value(rest, indent, l.no)
		if err != nil {
			return nil, err
		}
		node.seq = append(node.seq, item)
	}
	return node, nil
}

func (p *yamlParser) value(rest string, indent, no int) (*yamlNode, error) {
	anchor := ""
	if strings.HasPrefix(rest, "&") {
		name, remainder, _ := strings.Cut(rest[1:], " ")
		if !yamlAliasName.MatchString(name) {
			return nil, fmt.Errorf("line %d has an invalid anchor", no)
		}
		anchor, rest = name, strings.TrimSpace(remainder)
	}
	var node *yamlNode
	switch {
	case rest == "":
		if p.i < len(p.lines) && p.lines[p.i].indent > indent {
			var err error
			if node, err = p.block(p.lines[p.i].indent); err != nil {
				return nil, err
			}
		} else if p.i < len(p.lines) && p.lines[p.i].indent == indent && yamlSeqItem(p.lines[p.i].text) {
			var err error
			if node, err = p.sequence(indent); err != nil {
				return nil, err
			}
		} else {
			node = &yamlNode{kind: yamlScalar}
		}
	case rest[0] == '*':
		if anchor != "" || !yamlAliasName.MatchString(rest[1:]) {
			return nil, fmt.Errorf("line %d has an invalid alias", no)
		}
		node = p.anchors[rest[1:]]
		if node == nil {
			return nil, fmt.Errorf("line %d names an unknown anchor %s", no, rest[1:])
		}
	case strings.IndexByte("|>!%@`?", rest[0]) >= 0:
		return nil, fmt.Errorf("line %d uses YAML the agent does not read (%c)", no, rest[0])
	case rest[0] == '[' || rest[0] == '{':
		if !balancedYAMLFlow(rest) {
			return nil, fmt.Errorf("line %d has a flow collection spanning lines", no)
		}
		node = &yamlNode{kind: yamlScalar, value: rest}
	case rest[0] == '"' || rest[0] == '\'':
		text, err := unquoteYAML(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", no, err)
		}
		node = &yamlNode{kind: yamlScalar, value: text}
	default:
		if strings.Contains(rest, ": ") || strings.HasSuffix(rest, ":") {
			return nil, fmt.Errorf("line %d has a nested mapping on one line", no)
		}
		node = &yamlNode{kind: yamlScalar, value: rest}
		if rest == "~" || rest == "null" || rest == "Null" || rest == "NULL" {
			node.value = ""
		}
	}
	if node.kind == yamlScalar && rest != "" && p.i < len(p.lines) && p.lines[p.i].indent > indent {
		return nil, fmt.Errorf("line %d continues a value over several lines", p.lines[p.i].no)
	}
	if anchor != "" {
		p.anchors[anchor] = node
	}
	return node, nil
}

func balancedYAMLFlow(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth == 0
}

// splitYAMLKey splits "key: value" and "key:".
func splitYAMLKey(text string) (key, rest string, err error) {
	if text != "" && (text[0] == '"' || text[0] == '\'') {
		end := closingYAMLQuote(text)
		if end < 0 {
			return "", "", errors.New("a quoted key is not closed")
		}
		key, err = unquoteYAML(text[:end+1])
		if err != nil {
			return "", "", err
		}
		after := text[end+1:]
		if after != ":" && !strings.HasPrefix(after, ": ") {
			return "", "", errors.New("a quoted key is not followed by a colon")
		}
		return key, strings.TrimSpace(after[1:]), nil
	}
	idx := strings.Index(text, ": ")
	if idx < 0 {
		if !strings.HasSuffix(text, ":") {
			return "", "", errors.New("a line is not a setting")
		}
		idx = len(text) - 1
	}
	key = strings.TrimSpace(text[:idx])
	if key == "" || strings.IndexByte("&*!|>%@`?{[-#", key[0]) >= 0 && key != "<<" {
		return "", "", errors.New("a key uses YAML the agent does not read")
	}
	return key, strings.TrimSpace(text[idx+1:]), nil
}

func closingYAMLQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

func unquoteYAML(s string) (string, error) {
	end := closingYAMLQuote(s)
	if end != len(s)-1 {
		return "", errors.New("a quoted value is not closed or has text after it")
	}
	body := s[1:end]
	if s[0] == '\'' {
		return strings.ReplaceAll(body, "''", "'"), nil
	}
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			out.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", errors.New("a quoted value ends in an escape")
		}
		switch body[i] {
		case '\\', '"', '/':
			out.WriteByte(body[i])
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		default:
			return "", fmt.Errorf("a quoted value uses the escape \\%c", body[i])
		}
	}
	return out.String(), nil
}
