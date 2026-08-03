package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudapp3/vmflow/internal/i18n"
)

func TestComputeNetworkRatesHandlesResetAndMissingInterfaces(t *testing.T) {
	m := managedTestModel()
	m.stats = &StatsResponse{HostNetwork: &HostNetworkSnapshot{Interfaces: []HostNetworkInterface{
		{Name: "eth0", CountersAvailable: true, RXBytes: 100, TXBytes: 200},
		{Name: "reset0", CountersAvailable: true, RXBytes: 500, TXBytes: 700},
		{Name: "gone0", CountersAvailable: true, RXBytes: 10, TXBytes: 20},
	}}}
	m.networkRates["gone0"] = rateEntry{UploadRate: 5, DownloadRate: 6}

	m.computeNetworkRates(&HostNetworkSnapshot{Interfaces: []HostNetworkInterface{
		{Name: "eth0", CountersAvailable: true, RXBytes: 160, TXBytes: 240},
		{Name: "reset0", CountersAvailable: true, RXBytes: 20, TXBytes: 720},
		{Name: "new0", CountersAvailable: true, RXBytes: 30, TXBytes: 40},
	}}, 2)

	if got := m.networkRates["eth0"]; got.DownloadRate != 30 || got.UploadRate != 20 {
		t.Fatalf("eth0 rate = %+v, want download 30 and upload 20", got)
	}
	if got := m.networkRates["reset0"]; got.DownloadRate != 0 || got.UploadRate != 10 {
		t.Fatalf("reset rate = %+v, want download zero and upload 10", got)
	}
	if got := m.networkRates["new0"]; got.DownloadRate != 0 || got.UploadRate != 0 {
		t.Fatalf("new interface rate = %+v, want zero", got)
	}
	if _, exists := m.networkRates["gone0"]; exists {
		t.Fatal("missing interface rate was not removed")
	}

	m.computeNetworkRates(nil, 2)
	if len(m.networkRates) != 0 {
		t.Fatalf("rates after unsupported snapshot = %+v, want empty", m.networkRates)
	}
}

func TestDashboardNetworkJumpAndInterfaceToggle(t *testing.T) {
	m := managedTestModel()
	m.stats = &StatsResponse{HostNetwork: testHostNetworkSnapshot()}

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	m = updated.(Model)
	if m.view != viewDashboard || m.contentYOffset == 0 || m.contentYOffset != m.dashboardNetworkYOffset() {
		t.Fatalf("network jump = view %v offset %d, want dashboard offset %d", m.view, m.contentYOffset, m.dashboardNetworkYOffset())
	}
	if output := ansi.Strip(m.View()); !strings.Contains(output, "Host Network") {
		t.Fatalf("network jump did not reveal host network section:\n%s", output)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	if !m.networkExpanded || !strings.Contains(m.statusText, "all interfaces") || m.contentYOffset != m.dashboardNetworkYOffset() {
		t.Fatalf("expanded state = %v offset = %d status = %q", m.networkExpanded, m.contentYOffset, m.statusText)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	if m.networkExpanded || !strings.Contains(m.statusText, "active physical") {
		t.Fatalf("collapsed state = %v status = %q", m.networkExpanded, m.statusText)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = updated.(Model)
	if m.view != viewDashboard || m.contentYOffset != 0 {
		t.Fatalf("dashboard navigation did not return to top: view=%v offset=%d", m.view, m.contentYOffset)
	}
}

func TestDashboardNetworkSectionHandlesOldAndPartiallyUnavailableDaemons(t *testing.T) {
	t.Run("old daemon", func(t *testing.T) {
		m := managedTestModel()
		m.stats = &StatsResponse{}
		m.contentYOffset = m.dashboardNetworkYOffset()
		output := ansi.Strip(m.View())
		if !strings.Contains(output, "does not report host network data") {
			t.Fatalf("old daemon state is unclear:\n%s", output)
		}
		if strings.Contains(output, "OFFLINE") {
			t.Fatalf("old daemon was rendered offline:\n%s", output)
		}
		assertRenderBounds(t, m.View(), m.width, m.height)
	})

	t.Run("partial collection", func(t *testing.T) {
		m := managedTestModelWithTranslator(i18n.New(i18n.SimplifiedChinese))
		m.stats = &StatsResponse{HostNetwork: &HostNetworkSnapshot{
			Available: false,
			OS:        "linux",
			Arch:      "amd64",
			Warnings:  []string{"interfaces_unavailable", "counters_unavailable"},
		}}
		m.contentYOffset = m.dashboardNetworkYOffset()
		output := ansi.Strip(m.View())
		for _, expected := range []string{"本机网络信息不可用", "接口元数据不可用", "流量计数不可用"} {
			if !strings.Contains(output, expected) {
				t.Fatalf("partial state omitted %q:\n%s", expected, output)
			}
		}
		if strings.Contains(output, "interfaces_unavailable") || strings.Contains(output, "tui.") {
			t.Fatalf("partial state leaked internal keys:\n%s", output)
		}
		assertRenderBounds(t, m.View(), m.width, m.height)
	})
}

func TestDashboardWithNetworkFitsRepresentativeTerminalSizes(t *testing.T) {
	locales := []string{i18n.English, i18n.SimplifiedChinese}
	sizes := []struct {
		width  int
		height int
	}{{40, 12}, {60, 20}, {80, 24}, {100, 30}}

	for _, locale := range locales {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s_%dx%d", locale, size.width, size.height), func(t *testing.T) {
				m := managedTestModelWithTranslator(i18n.New(locale))
				m.width = size.width
				m.height = size.height
				m.networkExpanded = true
				m.stats = &StatsResponse{HostNetwork: testHostNetworkSnapshot()}

				top := m.View()
				assertRenderBounds(t, top, size.width, size.height)
				if size.width == 40 && size.height == 12 {
					plainTop := ansi.Strip(top)
					for _, expected := range []string{m.t("tui.panel.overview"), m.t("tui.label.host_rx"), "eth0"} {
						if !strings.Contains(plainTop, expected) {
							t.Fatalf("minimum dashboard first screen omitted %q:\n%s", expected, plainTop)
						}
					}
				}
				full := ansi.Strip(m.renderDashboard())
				if strings.Contains(full, "tui.") || strings.Contains(full, "network.warning.") {
					t.Fatalf("dashboard leaked a translation key:\n%s", full)
				}
				for _, expected := range []string{m.t("tui.panel.host_network"), m.t("tui.network.interfaces")} {
					if !strings.Contains(full, expected) {
						t.Fatalf("dashboard omitted %q:\n%s", expected, full)
					}
				}

				updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
				m = updated.(Model)
				if m.contentYOffset == 0 || m.contentYOffset != m.contentMaxYOffset() {
					t.Fatalf("dashboard end offset = %d, max = %d", m.contentYOffset, m.contentMaxYOffset())
				}
				bottom := m.View()
				assertRenderBounds(t, bottom, size.width, size.height)
				if bottom == top {
					t.Fatal("dashboard viewport did not move after End")
				}
			})
		}
	}
}

func TestDashboardShowsManagedHostPrimaryNetwork(t *testing.T) {
	m := managedTestModel()
	m.stats = &StatsResponse{HostNetwork: testHostNetworkSnapshot()}
	m.networkExpanded = true
	m.networkRates["docker0"] = rateEntry{DownloadRate: 1_000_000, UploadRate: 1_000_000}

	output := ansi.Strip(m.renderSystemPanel())
	for _, expected := range []string{"vm-edge-01", "Primary Net", "eth0", "10.20.30.40/24"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("system panel omitted %q:\n%s", expected, output)
		}
	}
}

func testHostNetworkSnapshot() *HostNetworkSnapshot {
	interfaces := []HostNetworkInterface{
		{
			Index:             1,
			Name:              "eth0",
			Up:                true,
			Running:           true,
			MTU:               1500,
			CountersAvailable: true,
			RXBytes:           12_345_678,
			TXBytes:           8_765_432,
			Addresses: []HostNetworkAddress{
				{CIDR: "10.20.30.40/24", Family: "ipv4", Scope: "private"},
				{CIDR: "2001:db8:1234:5678:90ab:cdef:1234:5678/64", Family: "ipv6", Scope: "global_unicast"},
				{CIDR: "fe80::1234:5678:90ab:cdef/64", Family: "ipv6", Scope: "link_local", Zone: "eth0"},
			},
		},
		{
			Index:             2,
			Name:              "Ethernet Adapter With A Very Long Descriptive Name",
			Up:                true,
			Running:           true,
			MTU:               1500,
			CountersAvailable: true,
			RXBytes:           4_096,
			TXBytes:           8_192,
			Addresses: []HostNetworkAddress{
				{CIDR: "192.168.50.10/24", Family: "ipv4", Scope: "private"},
			},
		},
		{
			Index:             3,
			Name:              "docker0",
			Up:                true,
			Running:           true,
			MTU:               1500,
			CountersAvailable: true,
			RXBytes:           2_048,
			TXBytes:           4_096,
			Addresses: []HostNetworkAddress{
				{CIDR: "172.17.0.1/16", Family: "ipv4", Scope: "private"},
			},
		},
		{
			Index:             4,
			Name:              "lo",
			Up:                true,
			Running:           true,
			Loopback:          true,
			MTU:               65_536,
			CountersAvailable: true,
			RXBytes:           1_024,
			TXBytes:           1_024,
			Addresses: []HostNetworkAddress{
				{CIDR: "127.0.0.1/8", Family: "ipv4", Scope: "loopback"},
				{CIDR: "::1/128", Family: "ipv6", Scope: "loopback"},
			},
		},
	}
	for index := 0; index < 8; index++ {
		interfaces = append(interfaces, HostNetworkInterface{
			Index:     10 + index,
			Name:      fmt.Sprintf("veth-workload-%02d-with-long-name", index),
			MTU:       1500,
			Addresses: []HostNetworkAddress{},
		})
	}
	return &HostNetworkSnapshot{
		Available:  true,
		Hostname:   "vm-edge-01",
		OS:         "linux",
		Arch:       "amd64",
		Interfaces: interfaces,
		Warnings:   []string{"addresses_unavailable"},
	}
}
