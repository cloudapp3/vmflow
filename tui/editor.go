package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/internal/i18n"
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
	fieldRuleID            = "rule_id"
	fieldName              = "name"
	fieldProtocol          = "protocol"
	fieldListenAddr        = "listen_addr"
	fieldListenPort        = "listen_port"
	fieldTargetAddr        = "target_addr"
	fieldTargetPort        = "target_port"
	fieldEnabled           = "enabled"
	fieldSpeedLimit        = "speed_limit"
	fieldBandwidthRate     = "bandwidth_rate"
	fieldBandwidthUpload   = "bandwidth_upload"
	fieldBandwidthDownload = "bandwidth_download"
	fieldQuotaDaily        = "quota_daily"
	fieldQuotaMonthly      = "quota_monthly"
	fieldQuotaTimezone     = "quota_timezone"
	fieldMaxConn           = "max_conn"
	fieldIdleTimeout       = "idle_timeout"
	fieldSourceIPMode      = "source_ip_mode"
	fieldSourceIPs         = "source_ips"
	fieldRemark            = "remark"
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
	tr         *i18n.Translator
	kind       editorKind
	originalID string
	base       RuleInfo
	fields     []editorField
	focus      int
	advanced   bool
	errors     map[string]string
	existing   map[string]struct{}
	initial    map[string]string
	autoRuleID string
}

func newRuleEditor(kind editorKind, rule RuleInfo, rules []RuleInfo) *ruleEditor {
	return newRuleEditorWithTranslator(kind, rule, rules, i18n.New(i18n.English))
}

func newRuleEditorWithTranslator(kind editorKind, rule RuleInfo, rules []RuleInfo, translator *i18n.Translator) *ruleEditor {
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	editor := &ruleEditor{
		tr:         translator,
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
	bandwidthRate, bandwidthUpload, bandwidthDownload := "", "", ""
	if rule.Bandwidth != nil {
		bandwidthRate = formatEditorByteRate(rule.Bandwidth.Rate)
		bandwidthUpload = formatEditorByteRate(rule.Bandwidth.Upload)
		bandwidthDownload = formatEditorByteRate(rule.Bandwidth.Download)
	}
	quotaDaily, quotaMonthly := "", ""
	quotaTimezone := "UTC"
	if translator.Locale() == i18n.SimplifiedChinese {
		quotaTimezone = "Asia/Shanghai"
	}
	if rule.TrafficQuota != nil {
		quotaDaily = formatEditorByteSize(rule.TrafficQuota.Daily)
		quotaMonthly = formatEditorByteSize(rule.TrafficQuota.Monthly)
		if strings.TrimSpace(rule.TrafficQuota.Timezone) != "" {
			quotaTimezone = rule.TrafficQuota.Timezone
		}
	}
	editor.fields = []editorField{
		newTextField(fieldRuleID, translator.T("tui.editor.field.rule_id"), rule.RuleID, ruleIDKind, 64),
		newTextField(fieldName, translator.T("tui.editor.field.name"), rule.Name, editorText, 80),
		{key: fieldProtocol, label: translator.T("tui.editor.field.protocol"), kind: editorProtocol, choice: protocol},
		newTextField(fieldListenAddr, translator.T("tui.editor.field.listen_address"), rule.ListenAddr, editorText, 255),
		newTextField(fieldListenPort, translator.T("tui.editor.field.listen_port"), formatEditorInt(rule.ListenPort), editorText, 5),
		newTextField(fieldTargetAddr, translator.T("tui.editor.field.target_address"), rule.TargetAddr, editorText, 255),
		newTextField(fieldTargetPort, translator.T("tui.editor.field.target_port"), formatEditorInt(rule.TargetPort), editorText, 5),
		{key: fieldEnabled, label: translator.T("tui.editor.field.enabled"), kind: editorToggle, enabled: rule.Enabled},
		newTextField(fieldBandwidthRate, translator.T("tui.editor.field.bandwidth_rate"), bandwidthRate, editorText, 32),
		newTextField(fieldQuotaDaily, translator.T("tui.editor.field.quota_daily"), quotaDaily, editorText, 32),
		newTextField(fieldQuotaMonthly, translator.T("tui.editor.field.quota_monthly"), quotaMonthly, editorText, 32),
		newTextField(fieldBandwidthUpload, translator.T("tui.editor.field.bandwidth_upload"), bandwidthUpload, editorText, 32),
		newTextField(fieldBandwidthDownload, translator.T("tui.editor.field.bandwidth_download"), bandwidthDownload, editorText, 32),
		newTextField(fieldQuotaTimezone, translator.T("tui.editor.field.quota_timezone"), quotaTimezone, editorText, 64),
		newTextField(fieldSpeedLimit, translator.T("tui.editor.field.speed_limit"), strconv.FormatInt(rule.SpeedLimit, 10), editorText, 18),
		newTextField(fieldMaxConn, translator.T("tui.editor.field.max_connections"), strconv.Itoa(rule.MaxConn), editorText, 10),
		newTextField(fieldIdleTimeout, translator.T("tui.editor.field.idle_timeout"), strconv.Itoa(rule.IdleTimeout), editorText, 10),
		{key: fieldSourceIPMode, label: translator.T("tui.editor.field.source_ip_mode"), kind: editorSourceIPMode, choice: sourceIPMode},
		newTextField(fieldSourceIPs, translator.T("tui.editor.field.source_ips"), strings.Join(rule.SourceIPs, ", "), editorText, 16384),
		newTextField(fieldRemark, translator.T("tui.editor.field.remark"), rule.Remark, editorText, 240),
	}
	editor.initial = make(map[string]string, len(editor.fields))
	for _, field := range editor.fields {
		editor.initial[field.key] = editor.value(field.key)
	}
	editor.advanced = kind != editorCreate && hasAdvancedRuleSettings(rule)
	if kind == editorCreate {
		editor.autoRuleID = strings.TrimSpace(rule.RuleID)
	}
	editor.focus = editor.firstFocusable()
	if kind == editorCreate && editor.autoRuleID != "" {
		editor.focus = editor.fieldIndex(fieldName)
	}
	editor.syncFocus()
	return editor
}

func (e *ruleEditor) setTranslator(translator *i18n.Translator) {
	if e == nil {
		return
	}
	if translator == nil {
		translator = i18n.New(i18n.English)
	}
	hadErrors := len(e.errors) > 0
	e.tr = translator
	for index := range e.fields {
		key := editorFieldTranslationKey(e.fields[index].key)
		if key != "" {
			e.fields[index].label = translator.T(key)
		}
	}
	if hadErrors {
		_, _ = e.rule()
	}
}

func editorFieldTranslationKey(field string) string {
	switch field {
	case fieldRuleID:
		return "tui.editor.field.rule_id"
	case fieldName:
		return "tui.editor.field.name"
	case fieldProtocol:
		return "tui.editor.field.protocol"
	case fieldListenAddr:
		return "tui.editor.field.listen_address"
	case fieldListenPort:
		return "tui.editor.field.listen_port"
	case fieldTargetAddr:
		return "tui.editor.field.target_address"
	case fieldTargetPort:
		return "tui.editor.field.target_port"
	case fieldEnabled:
		return "tui.editor.field.enabled"
	case fieldSpeedLimit:
		return "tui.editor.field.speed_limit"
	case fieldBandwidthRate:
		return "tui.editor.field.bandwidth_rate"
	case fieldBandwidthUpload:
		return "tui.editor.field.bandwidth_upload"
	case fieldBandwidthDownload:
		return "tui.editor.field.bandwidth_download"
	case fieldQuotaDaily:
		return "tui.editor.field.quota_daily"
	case fieldQuotaMonthly:
		return "tui.editor.field.quota_monthly"
	case fieldQuotaTimezone:
		return "tui.editor.field.quota_timezone"
	case fieldMaxConn:
		return "tui.editor.field.max_connections"
	case fieldIdleTimeout:
		return "tui.editor.field.idle_timeout"
	case fieldSourceIPMode:
		return "tui.editor.field.source_ip_mode"
	case fieldSourceIPs:
		return "tui.editor.field.source_ips"
	case fieldRemark:
		return "tui.editor.field.remark"
	default:
		return ""
	}
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
	fieldKey := e.fields[e.focus].key
	before := e.fields[e.focus].input.Value()
	var cmd tea.Cmd
	e.fields[e.focus].input, cmd = e.fields[e.focus].input.Update(msg)
	after := e.fields[e.focus].input.Value()
	delete(e.errors, fieldKey)
	if fieldKey == fieldName && before != after {
		e.updateAutomaticRuleID(after)
	}
	return cmd
}

func (e *ruleEditor) moveFocus(delta int) {
	if e == nil || len(e.fields) == 0 {
		return
	}
	for attempts := 0; attempts < len(e.fields); attempts++ {
		e.focus = (e.focus + delta + len(e.fields)) % len(e.fields)
		if e.fieldVisible(e.focus) && e.fields[e.focus].kind != editorReadOnly {
			break
		}
	}
	e.syncFocus()
}

func (e *ruleEditor) firstFocusable() int {
	for index, field := range e.fields {
		if e.fieldVisible(index) && field.kind != editorReadOnly {
			return index
		}
	}
	return 0
}

func (e *ruleEditor) toggleAdvanced() {
	if e == nil {
		return
	}
	e.advanced = !e.advanced
	if !e.fieldVisible(e.focus) {
		e.focus = e.firstFocusable()
	}
	e.syncFocus()
}

func (e *ruleEditor) fieldVisible(index int) bool {
	if e == nil || index < 0 || index >= len(e.fields) {
		return false
	}
	if e.advanced {
		return true
	}
	switch e.fields[index].key {
	case fieldRuleID, fieldName, fieldProtocol, fieldListenAddr, fieldListenPort, fieldTargetAddr, fieldTargetPort, fieldEnabled, fieldBandwidthRate, fieldQuotaDaily, fieldQuotaMonthly:
		return true
	default:
		return false
	}
}

func (e *ruleEditor) visibleFieldIndices() []int {
	if e == nil {
		return nil
	}
	indices := make([]int, 0, len(e.fields))
	for index := range e.fields {
		if e.fieldVisible(index) {
			indices = append(indices, index)
		}
	}
	return indices
}

func (e *ruleEditor) focusField(key string) bool {
	if e == nil {
		return false
	}
	for index := range e.fields {
		if e.fields[index].key != key {
			continue
		}
		if !e.fieldVisible(index) {
			e.advanced = true
		}
		if e.fields[index].kind == editorReadOnly {
			return false
		}
		e.focus = index
		e.syncFocus()
		return true
	}
	return false
}

func (e *ruleEditor) updateAutomaticRuleID(name string) {
	if e == nil || e.kind != editorCreate {
		return
	}
	index := e.fieldIndex(fieldRuleID)
	if index < 0 {
		return
	}
	current := strings.TrimSpace(e.fields[index].input.Value())
	if current != "" && current != e.autoRuleID {
		return
	}
	base := ruleIDSlug(name)
	if base == "" {
		return
	}
	next := availableRuleID(base, e.existing)
	e.fields[index].input.SetValue(next)
	e.autoRuleID = next
	delete(e.errors, fieldRuleID)
}

func (e *ruleEditor) fieldIndex(key string) int {
	if e == nil {
		return -1
	}
	for index := range e.fields {
		if e.fields[index].key == key {
			return index
		}
	}
	return -1
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
	rule.Bandwidth = nil
	bandwidthRate := e.parseByteRate(fieldBandwidthRate)
	bandwidthUpload := e.parseByteRate(fieldBandwidthUpload)
	bandwidthDownload := e.parseByteRate(fieldBandwidthDownload)
	if bandwidthRate > 0 || bandwidthUpload > 0 || bandwidthDownload > 0 {
		rule.Bandwidth = &engine.BandwidthLimit{Rate: bandwidthRate, Upload: bandwidthUpload, Download: bandwidthDownload}
	}
	if bandwidthRate > 0 && (bandwidthUpload > 0 || bandwidthDownload > 0) {
		message := e.tr.T("tui.validation.bandwidth_mode")
		e.errors[fieldBandwidthRate] = message
		if bandwidthUpload > 0 {
			e.errors[fieldBandwidthUpload] = message
		}
		if bandwidthDownload > 0 {
			e.errors[fieldBandwidthDownload] = message
		}
	}
	rule.TrafficQuota = nil
	quotaDaily := e.parseByteSize(fieldQuotaDaily)
	quotaMonthly := e.parseByteSize(fieldQuotaMonthly)
	if quotaDaily > 0 || quotaMonthly > 0 {
		timezone := strings.TrimSpace(e.value(fieldQuotaTimezone))
		if timezone == "" {
			timezone = "UTC"
		}
		rule.TrafficQuota = &engine.TrafficQuota{
			Daily: quotaDaily, Monthly: quotaMonthly,
			Timezone: timezone, Action: "block",
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			e.errors[fieldQuotaTimezone] = e.tr.T("tui.validation.quota_timezone")
		}
	}
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
		e.errors[fieldRuleID] = e.tr.T("tui.validation.required")
	} else if _, exists := e.existing[rule.RuleID]; exists && rule.RuleID != e.originalID {
		e.errors[fieldRuleID] = e.tr.T("tui.validation.already_exists")
	}
	if rule.Name == "" {
		e.errors[fieldName] = e.tr.T("tui.validation.required")
	}
	if !validEditorProtocol(string(rule.Protocol)) {
		e.errors[fieldProtocol] = e.tr.T("tui.validation.protocol")
	}
	if !validEditorSourceIPMode(e.value(fieldSourceIPMode)) {
		e.errors[fieldSourceIPMode] = e.tr.T("tui.validation.source_ip_mode")
	} else if rule.SourceIPMode != "" {
		switch {
		case len(rule.SourceIPs) == 0:
			e.errors[fieldSourceIPs] = e.tr.T("tui.validation.source_ip_required")
		case len(rule.SourceIPs) > engine.MaxSourceIPsPerRule:
			e.errors[fieldSourceIPs] = e.tr.Tf("tui.validation.maximum_entries", engine.MaxSourceIPsPerRule)
		default:
			for _, value := range rule.SourceIPs {
				if _, err := engine.ParseSourceIPPrefix(value); err != nil {
					e.errors[fieldSourceIPs] = e.tr.Tf("tui.validation.source_ip_invalid", value)
					break
				}
			}
		}
	}
	if rule.TargetAddr == "" {
		e.errors[fieldTargetAddr] = e.tr.T("tui.validation.required")
	}
	if len(e.errors) == 0 {
		if err := rule.Validate(); err != nil {
			if e.tr.Locale() == i18n.English {
				e.errors["form"] = err.Error()
			} else if strings.Contains(strings.ToLower(err.Error()), "disabled in this build") {
				e.errors["form"] = e.tr.T("tui.validation.protocol_disabled")
			} else {
				e.errors["form"] = e.tr.T("tui.validation.rule_invalid")
			}
		}
	}
	return rule.Standardize(), len(e.errors) == 0
}

func (e *ruleEditor) parseInt(key string, minValue, maxValue int) int {
	value := strings.TrimSpace(e.value(key))
	parsed, err := strconv.Atoi(value)
	if err != nil {
		e.errors[key] = e.tr.T("tui.validation.integer")
		return 0
	}
	if parsed < minValue || parsed > maxValue {
		e.errors[key] = e.tr.Tf("tui.validation.range", minValue, maxValue)
	}
	return parsed
}

func (e *ruleEditor) parseInt64(key string, minValue int64) int64 {
	value := strings.TrimSpace(e.value(key))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		e.errors[key] = e.tr.T("tui.validation.integer")
		return 0
	}
	if parsed < minValue {
		e.errors[key] = e.tr.Tf("tui.validation.minimum", minValue)
	}
	return parsed
}

func (e *ruleEditor) parseByteRate(key string) engine.ByteRate {
	value := strings.TrimSpace(e.value(key))
	if value == "" {
		return 0
	}
	var parsed engine.ByteRate
	if err := parsed.UnmarshalText([]byte(value)); err != nil {
		e.errors[key] = e.tr.T("tui.validation.bandwidth")
		return 0
	}
	return parsed
}

func (e *ruleEditor) parseByteSize(key string) engine.ByteSize {
	value := strings.TrimSpace(e.value(key))
	if value == "" {
		return 0
	}
	var parsed engine.ByteSize
	if err := parsed.UnmarshalText([]byte(value)); err != nil {
		e.errors[key] = e.tr.T("tui.validation.traffic_quota")
		return 0
	}
	return parsed
}

func (e *ruleEditor) title() string {
	if e == nil {
		return "Rule Editor"
	}
	switch e.kind {
	case editorCreate:
		return e.tr.T("tui.editor.title.add")
	case editorCopy:
		return e.tr.T("tui.editor.title.copy")
	default:
		return e.tr.T("tui.editor.title.edit")
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

func hasAdvancedRuleSettings(rule RuleInfo) bool {
	return rule.SpeedLimit != 0 || rule.MaxConn != 0 || rule.IdleTimeout != 0 ||
		rule.SourceIPMode != "" || len(rule.SourceIPs) != 0 || strings.TrimSpace(rule.Remark) != "" ||
		(rule.Bandwidth != nil && (rule.Bandwidth.Upload > 0 || rule.Bandwidth.Download > 0))
}

func ruleIDSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	separator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			separator = false
		default:
			separator = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func availableRuleID(base string, existing map[string]struct{}) string {
	base = strings.Trim(strings.TrimSpace(base), "-")
	if base == "" {
		base = "new-rule"
	}
	if _, found := existing[base]; !found {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, found := existing[candidate]; !found {
			return candidate
		}
	}
}

func newRuleID(rules []RuleInfo) string {
	existing := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		existing[rule.RuleID] = struct{}{}
	}
	return availableRuleID("new-rule", existing)
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

func formatEditorByteRate(value engine.ByteRate) string {
	if value <= 0 {
		return ""
	}
	return value.String()
}

func formatEditorByteSize(value engine.ByteSize) string {
	if value <= 0 {
		return ""
	}
	return value.String()
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
	if source.Bandwidth != nil {
		value := *source.Bandwidth
		result.Bandwidth = &value
	}
	if source.TrafficQuota != nil {
		value := *source.TrafficQuota
		result.TrafficQuota = &value
	}
	return result
}
