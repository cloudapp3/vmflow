package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestDetailScrollReachesTrafficAndResets(t *testing.T) {
	newDetailModel := func(t *testing.T) Model {
		t.Helper()
		m := managedTestModel()
		m.width = 60
		m.height = 20
		m.view = viewDetail
		m.selectedRuleID = "disabled"
		if m.detailMaxYOffset() == 0 {
			t.Fatal("60x20 detail unexpectedly fits without scrolling")
		}
		return m
	}

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEnd},
		{Type: tea.KeyPgDown},
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := newDetailModel(t)
			updated, cmd := m.handleKey(key)
			m = updated.(Model)
			if cmd != nil || m.contentYOffset == 0 {
				t.Fatalf("%s did not scroll detail: offset=%d cmd=%v", key.String(), m.contentYOffset, cmd)
			}

			output := ansi.Strip(m.View())
			if !strings.Contains(output, "Traffic") || !strings.Contains(output, "Download") {
				t.Fatalf("%s did not reveal bottom traffic fields:\n%s", key.String(), output)
			}
		})
	}

	t.Run("escape and re-enter reset", func(t *testing.T) {
		m := newDetailModel(t)
		updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
		m = updated.(Model)
		if m.contentYOffset == 0 {
			t.Fatal("precondition failed: End did not scroll detail")
		}

		updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		m = updated.(Model)
		if cmd != nil || m.view != viewRules || m.contentYOffset != 0 {
			t.Fatalf("escape did not reset detail scroll: view=%v offset=%d cmd=%v", m.view, m.contentYOffset, cmd)
		}

		// Enter must reset any stale offset before opening another detail view.
		m.contentYOffset = 3
		updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		m = updated.(Model)
		if cmd != nil || m.view != viewDetail || m.contentYOffset != 0 {
			t.Fatalf("re-enter did not reset detail scroll: view=%v offset=%d cmd=%v", m.view, m.contentYOffset, cmd)
		}
	})

	t.Run("selection change resets", func(t *testing.T) {
		m := newDetailModel(t)
		updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
		m = updated.(Model)
		previousRuleID := m.selectedRuleID
		m.moveSelection(1)
		if m.selectedRuleID == previousRuleID {
			t.Fatalf("precondition failed: selection did not change from %q", previousRuleID)
		}
		if m.contentYOffset != 0 {
			t.Fatalf("selection change retained detail offset %d", m.contentYOffset)
		}
	})
}

func TestHelpScrollReachesSyncWithFixedFooterAndReturnsHome(t *testing.T) {
	m := managedTestModel()
	m.width = 80
	m.height = 24
	m.overlay = overlayHelp
	m.overlayYOffset = 0
	if m.helpMaxYOffset() == 0 {
		t.Fatal("80x24 help unexpectedly fits without scrolling")
	}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	if cmd != nil || m.overlayYOffset != m.helpMaxYOffset() || m.overlayYOffset == 0 {
		t.Fatalf("End did not reach help bottom: offset=%d max=%d cmd=%v", m.overlayYOffset, m.helpMaxYOffset(), cmd)
	}
	output := ansi.Strip(m.View())
	if !strings.Contains(output, "Sync") || !strings.Contains(output, "VALIDATED") || !strings.Contains(output, "STALE") {
		t.Fatalf("help bottom omitted sync legend:\n%s", output)
	}
	for _, hint := range []string{"[home/end]jump", "[esc/?/F1]close"} {
		if !strings.Contains(output, hint) {
			t.Fatalf("scrolled help omitted fixed footer %q:\n%s", hint, output)
		}
	}
	assertRenderBounds(t, m.View(), m.width, m.height)

	updated, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(Model)
	if cmd != nil || m.overlayYOffset != 0 {
		t.Fatalf("Home did not reset help offset: offset=%d cmd=%v", m.overlayYOffset, cmd)
	}
	output = ansi.Strip(m.View())
	if !strings.Contains(output, "Keyboard Shortcuts") || !strings.Contains(output, "Quit") {
		t.Fatalf("Home did not restore help top:\n%s", output)
	}
}

func TestMouseWheelScrollsHelpAndDetail(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		m := managedTestModel()
		m.width = 80
		m.height = 24
		m.overlay = overlayHelp

		updated, cmd := m.Update(mouseWheelMsg(tea.MouseWheelDown, tea.MouseButtonWheelDown))
		m = updated.(Model)
		if cmd != nil || m.overlayYOffset == 0 {
			t.Fatalf("mouse wheel did not scroll help: offset=%d cmd=%v", m.overlayYOffset, cmd)
		}
		previousOffset := m.overlayYOffset
		updated, cmd = m.Update(mouseWheelMsg(tea.MouseWheelUp, tea.MouseButtonWheelUp))
		m = updated.(Model)
		if cmd != nil || m.overlayYOffset >= previousOffset {
			t.Fatalf("mouse wheel up did not reverse help scroll: before=%d after=%d cmd=%v", previousOffset, m.overlayYOffset, cmd)
		}
	})

	t.Run("detail", func(t *testing.T) {
		m := managedTestModel()
		m.width = 60
		m.height = 20
		m.view = viewDetail
		m.selectedRuleID = "disabled"

		updated, cmd := m.Update(mouseWheelMsg(tea.MouseWheelDown, tea.MouseButtonWheelDown))
		m = updated.(Model)
		if cmd != nil || m.contentYOffset == 0 {
			t.Fatalf("mouse wheel did not scroll detail: offset=%d cmd=%v", m.contentYOffset, cmd)
		}
		previousOffset := m.contentYOffset
		updated, cmd = m.Update(mouseWheelMsg(tea.MouseWheelUp, tea.MouseButtonWheelUp))
		m = updated.(Model)
		if cmd != nil || m.contentYOffset >= previousOffset {
			t.Fatalf("mouse wheel up did not reverse detail scroll: before=%d after=%d cmd=%v", previousOffset, m.contentYOffset, cmd)
		}
	})
}

func TestScrollableViewsFitMinimumSupportedTerminal(t *testing.T) {
	t.Run("detail", func(t *testing.T) {
		m := managedTestModel()
		m.width = 40
		m.height = 12
		m.view = viewDetail
		m.selectedRuleID = "disabled"
		m.contentYOffset = m.detailMaxYOffset()

		assertRenderBounds(t, m.View(), m.width, m.height)
	})

	t.Run("help", func(t *testing.T) {
		m := managedTestModel()
		m.width = 40
		m.height = 12
		m.overlay = overlayHelp
		m.overlayYOffset = m.helpMaxYOffset()

		assertRenderBounds(t, m.View(), m.width, m.height)
	})
}

func TestWindowResizeClampsScrollOffsets(t *testing.T) {
	m := managedTestModel()
	m.width = 120
	m.height = 40
	m.view = viewDetail
	m.overlay = overlayHelp
	m.contentYOffset = 1 << 20
	m.overlayYOffset = 1 << 20

	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = updated.(Model)
	if cmd != nil {
		t.Fatalf("resize returned unexpected command: %v", cmd)
	}
	if m.contentYOffset != m.detailMaxYOffset() {
		t.Fatalf("detail offset after resize = %d, want max %d", m.contentYOffset, m.detailMaxYOffset())
	}
	if m.overlayYOffset != m.helpMaxYOffset() {
		t.Fatalf("help offset after resize = %d, want max %d", m.overlayYOffset, m.helpMaxYOffset())
	}
}

func mouseWheelMsg(eventType tea.MouseEventType, button tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{
		Type:   eventType,
		Button: button,
		Action: tea.MouseActionPress,
	}
}
