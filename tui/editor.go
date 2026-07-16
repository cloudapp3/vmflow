package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
)

type editorKind int

const (
	editorCreate editorKind = iota
	editorEdit
	editorCopy
)

type editorFieldKind int

const (
	editorText editorFieldKind = iota
	editorProtocol
	editorSourceIPMode
	editorToggle
	editorReadOnly
)

const (
	fieldRuleID       = "rule_id"
	fieldName         = "name"
	fieldProtocol     = "protocol"
	fieldListenAddr   = "listen_addr"
	fieldListenPort   = "listen_port"
	fieldTargetAddr   = "target_addr"
	fieldTargetPort   = "target_port"
	fieldEnabled      = "enabled"
	fieldSpeedLimit   = "speed_limit"
	fieldMaxConn      = "max_conn"
	fieldIdleTimeout  = "idle_timeout"
	fieldSourceIPMode = "source_ip_mode"
	fieldSourceIPs    = "source_ips"
	fieldRemark       = "remark"
)

var editorProtocols = []string{"tcp", "udp", "tcp+udp"}
var editorSourceIPModes = []string{"off", "allowlist", "denylist"}

type editorField struct {
	key      string
	label    string
	kind     editorFieldKind
	input    textinput.Model
	choice   string
	enabled  bool
	maxWidth int
}

type ruleEditor struct {
	kind       editorKind
	originalID string
	base       RuleInfo
	fields     []editorField
	focus      int
	errors     map[string]string
	existing   map[string]struct{}
	initial    map[string]string
}

func newRuleEditor(kind editorKind, rule RuleInfo, rules []RuleInfo) *ruleEditor {
	editor := &ruleEditor{
		kind:       kind,
		originalID: rule.RuleID,
		base:       rule,
		errors:     make(map[string]string),
		existing:   make(map[string]struct{}, len(rules)),
	}
	for _, item := range rules {
		editor.existing[item.RuleID] = struct{}{}
	}

	protocol := strings.ToLower(strings.TrimSpace(string(rule.Protocol)))
	if !validEditorProtocol(protocol) {
		protocol = editorProtocols[0]
	}
	sourceIPMode := strings.ToLower(strings.TrimSpace(string(rule.SourceIPMode)))
	if !validEditorSourceIPMode(sourceIPMode) {
		sourceIPMode = string(engine.SourceIPModeOff)
	}
	ruleIDKind := editorText
	if kind == editorEdit {
		ruleIDKind = editorReadOnly
	}
	editor.fields = []editorField{
		newTextField(fieldRuleID, "Rule ID", rule.RuleID, ruleIDKind, 64),
		newTextField(fieldName, "Name", rule.Name, editorText, 80),
		{key: fieldProtocol, label: "Protocol", kind: editorProtocol, choice: protocol},
		newTextField(fieldListenAddr, "Listen address", rule.ListenAddr, editorText, 255),
		newTextField(fieldListenPort, "Listen port", formatEditorInt(rule.ListenPort), editorText, 5),
		newTextField(fieldTargetAddr, "Target address", rule.TargetAddr, editorText, 255),
		newTextField(fieldTargetPort, "Target port", formatEditorInt(rule.TargetPort), editorText, 5),
		{key: fieldEnabled, label: "Enabled", kind: editorToggle, enabled: rule.Enabled},
		newTextField(fieldSpeedLimit, "Speed limit (B/s)", strconv.FormatInt(rule.SpeedLimit, 10), editorText, 18),
		newTextField(fieldMaxConn, "Max connections", strconv.Itoa(rule.MaxConn), editorText, 10),
		newTextField(fieldIdleTimeout, "Idle timeout (s)", strconv.Itoa(rule.IdleTimeout), editorText, 10),
		{key: fieldSourceIPMode, label: "Source IP mode", kind: editorSourceIPMode, choice: sourceIPMode},
		newTextField(fieldSourceIPs, "Source IPs / CIDRs", strings.Join(rule.SourceIPs, ", "), editorText, 16384),
		newTextField(fieldRemark, "Remark", rule.Remark, editorText, 240),
	}
	editor.initial = make(map[string]string, len(editor.fields))
	for _, field := range editor.fields {
		editor.initial[field.key] = editor.value(field.key)
	}
	editor.focus = editor.firstFocusable()
	editor.syncFocus()
	return editor
}

func newTextField(key, label, value string, kind editorFieldKind, limit int) editorField {
	input := textinput.New()
	input.Prompt = ""
	input.SetValue(value)
	input.CharLimit = limit
	input.Width = 42
	return editorField{key: key, label: label, kind: kind, input: input, maxWidth: 42}
}

func (field editorField) inputView(available int) string {
	return responsiveTextInputView(field.input, available, field.maxWidth)
}

func responsiveTextInputView(input textinput.Model, available, maxWidth int) string {
	width := max(1, min(available, maxWidth))
	input.Width = max(width-lipgloss.Width(input.Prompt), 1)
	input.SetCursor(input.Position())
	return ansi.Truncate(input.View(), width, "")
}

func (e *ruleEditor) update(msg tea.KeyMsg) tea.Cmd {
	if e == nil || len(e.fields) == 0 {
		return nil
	}
	switch msg.String() {
	case "tab", "down", "enter":
		e.moveFocus(1)
		return nil
	case "shift+tab", "up":
		e.moveFocus(-1)
		return nil
	case "left":
		switch e.fields[e.focus].kind {
		case editorProtocol:
			e.cycleProtocol(-1)
			return nil
		case editorSourceIPMode:
			e.cycleSourceIPMode(-1)
			return nil
		}
	case "right":
		switch e.fields[e.focus].kind {
		case editorProtocol:
			e.cycleProtocol(1)
			return nil
		case editorSourceIPMode:
			e.cycleSourceIPMode(1)
			return nil
		}
	case " ":
		if e.fields[e.focus].kind == editorToggle {
			e.fields[e.focus].enabled = !e.fields[e.focus].enabled
			return nil
		}
	}
	if e.fields[e.focus].kind != editorText {
		return nil
	}
	var cmd tea.Cmd
	e.fields[e.focus].input, cmd = e.fields[e.focus].input.Update(msg)
	delete(e.errors, e.fields[e.focus].key)
	return cmd
}

func (e *ruleEditor) moveFocus(delta int) {
	if e == nil || len(e.fields) == 0 {
		return
	}
	for attempts := 0; attempts < len(e.fields); attempts++ {
		e.focus = (e.focus + delta + len(e.fields)) % len(e.fields)
		if e.fields[e.focus].kind != editorReadOnly {
			break
		}
	}
	e.syncFocus()
}

func (e *ruleEditor) firstFocusable() int {
	for index, field := range e.fields {
		if field.kind != editorReadOnly {
			return index
		}
	}
	return 0
}

func (e *ruleEditor) syncFocus() {
	for index := range e.fields {
		e.fields[index].input.Blur()
		if index == e.focus && e.fields[index].kind == editorText {
			e.fields[index].input.Focus()
		}
	}
}

func (e *ruleEditor) cycleProtocol(delta int) {
	current := e.fields[e.focus].choice
	index := 0
	for candidate, protocol := range editorProtocols {
		if protocol == current {
			index = candidate
			break
		}
	}
	index = (index + delta + len(editorProtocols)) % len(editorProtocols)
	e.fields[e.focus].choice = editorProtocols[index]
	delete(e.errors, fieldProtocol)
}

func (e *ruleEditor) cycleSourceIPMode(delta int) {
	current := e.fields[e.focus].choice
	index := 0
	for candidate, mode := range editorSourceIPModes {
		if mode == current {
			index = candidate
			break
		}
	}
	index = (index + delta + len(editorSourceIPModes)) % len(editorSourceIPModes)
	e.fields[e.focus].choice = editorSourceIPModes[index]
	delete(e.errors, fieldSourceIPMode)
	delete(e.errors, fieldSourceIPs)
	delete(e.errors, "form")
}

func (e *ruleEditor) value(key string) string {
	for index := range e.fields {
		if e.fields[index].key != key {
			continue
		}
		switch e.fields[index].kind {
		case editorProtocol, editorSourceIPMode:
			return e.fields[index].choice
		case editorToggle:
			return strconv.FormatBool(e.fields[index].enabled)
		default:
			return e.fields[index].input.Value()
		}
	}
	return ""
}

func (e *ruleEditor) dirty() bool {
	if e == nil {
		return false
	}
	for _, field := range e.fields {
		if e.value(field.key) != e.initial[field.key] {
			return true
		}
	}
	return false
}

func (e *ruleEditor) rule() (RuleInfo, bool) {
	if e == nil {
		return RuleInfo{}, false
	}
	e.errors = make(map[string]string)
	rule := e.base
	rule.RuleID = strings.TrimSpace(e.value(fieldRuleID))
	rule.Name = strings.TrimSpace(e.value(fieldName))
	rule.Protocol = engineProtocol(e.value(fieldProtocol))
	rule.ListenAddr = strings.TrimSpace(e.value(fieldListenAddr))
	rule.ListenPort = e.parseInt(fieldListenPort, 1, 65535)
	rule.TargetAddr = strings.TrimSpace(e.value(fieldTargetAddr))
	rule.TargetPort = e.parseInt(fieldTargetPort, 1, 65535)
	rule.Enabled = e.value(fieldEnabled) == "true"
	rule.SpeedLimit = e.parseInt64(fieldSpeedLimit, 0)
	rule.MaxConn = e.parseInt(fieldMaxConn, 0, int(^uint(0)>>1))
	rule.IdleTimeout = e.parseInt(fieldIdleTimeout, 0, int(^uint(0)>>1))
	rule.SourceIPMode = engine.SourceIPMode(strings.ToLower(strings.TrimSpace(e.value(fieldSourceIPMode))))
	if rule.SourceIPMode == engine.SourceIPModeOff {
		rule.SourceIPMode = ""
		rule.SourceIPs = nil
	} else {
		rule.SourceIPs = parseSourceIPs(e.value(fieldSourceIPs))
	}
	rule.Remark = strings.TrimSpace(e.value(fieldRemark))

	if rule.RuleID == "" {
		e.errors[fieldRuleID] = "required"
	} else if _, exists := e.existing[rule.RuleID]; exists && rule.RuleID != e.originalID {
		e.errors[fieldRuleID] = "already exists"
	}
	if rule.Name == "" {
		e.errors[fieldName] = "required"
	}
	if !validEditorProtocol(string(rule.Protocol)) {
		e.errors[fieldProtocol] = "choose tcp, udp, or tcp+udp"
	}
	if !validEditorSourceIPMode(e.value(fieldSourceIPMode)) {
		e.errors[fieldSourceIPMode] = "choose off, allowlist, or denylist"
	} else if rule.SourceIPMode != "" {
		switch {
		case len(rule.SourceIPs) == 0:
			e.errors[fieldSourceIPs] = "at least one IP or CIDR is required"
		case len(rule.SourceIPs) > engine.MaxSourceIPsPerRule:
			e.errors[fieldSourceIPs] = fmt.Sprintf("maximum %d entries", engine.MaxSourceIPsPerRule)
		default:
			for _, value := range rule.SourceIPs {
				if _, err := engine.ParseSourceIPPrefix(value); err != nil {
					e.errors[fieldSourceIPs] = err.Error()
					break
				}
			}
		}
	}
	if rule.TargetAddr == "" {
		e.errors[fieldTargetAddr] = "required"
	}
	if len(e.errors) == 0 {
		if err := rule.Validate(); err != nil {
			e.errors["form"] = err.Error()
		}
	}
	return rule.Standardize(), len(e.errors) == 0
}

func (e *ruleEditor) parseInt(key string, minValue, maxValue int) int {
	value := strings.TrimSpace(e.value(key))
	parsed, err := strconv.Atoi(value)
	if err != nil {
		e.errors[key] = "must be an integer"
		return 0
	}
	if parsed < minValue || parsed > maxValue {
		e.errors[key] = fmt.Sprintf("must be between %d and %d", minValue, maxValue)
	}
	return parsed
}

func (e *ruleEditor) parseInt64(key string, minValue int64) int64 {
	value := strings.TrimSpace(e.value(key))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		e.errors[key] = "must be an integer"
		return 0
	}
	if parsed < minValue {
		e.errors[key] = fmt.Sprintf("must be at least %d", minValue)
	}
	return parsed
}

func (e *ruleEditor) title() string {
	if e == nil {
		return "Rule Editor"
	}
	switch e.kind {
	case editorCreate:
		return "Add Rule"
	case editorCopy:
		return "Copy Rule"
	default:
		return "Edit Rule"
	}
}

func validEditorProtocol(protocol string) bool {
	for _, candidate := range editorProtocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}

func validEditorSourceIPMode(mode string) bool {
	for _, candidate := range editorSourceIPModes {
		if candidate == strings.ToLower(strings.TrimSpace(mode)) {
			return true
		}
	}
	return false
}

func parseSourceIPs(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func engineProtocol(value string) engine.Protocol {
	return engine.Protocol(strings.ToLower(strings.TrimSpace(value)))
}

func formatEditorInt(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func copyRule(source RuleInfo, rules []RuleInfo) RuleInfo {
	result := source
	baseID := strings.TrimSpace(source.RuleID) + "-copy"
	if baseID == "-copy" {
		baseID = "rule-copy"
	}
	existing := make(map[string]struct{}, len(rules))
	for _, item := range rules {
		existing[item.RuleID] = struct{}{}
	}
	result.RuleID = baseID
	for suffix := 2; ; suffix++ {
		if _, found := existing[result.RuleID]; !found {
			break
		}
		result.RuleID = fmt.Sprintf("%s-%d", baseID, suffix)
	}
	if result.Name == "" {
		result.Name = result.RuleID
	} else {
		result.Name += " copy"
	}
	result.Revision = 0
	result.CreatedTime = 0
	result.UpdatedTime = 0
	result.Domains = append([]string(nil), source.Domains...)
	result.SourceIPs = append([]string(nil), source.SourceIPs...)
	return result
}
