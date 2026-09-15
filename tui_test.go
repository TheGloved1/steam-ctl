package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func testModel() *model {
	o := defaultOptions()
	o.NoTUI = true
	m := &model{opts: o, selected: map[string]bool{}}
	m.apps = scanApps(o)
	m.refilter()
	return m
}

func keyPress(m *model, k string) *model {
	// build a KeyMsg like bubbletea does for runes / known keys
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	nm, _ := m.Update(msg)
	return nm.(*model)
}

func TestMenuMoveFlow(t *testing.T) {
	m := testModel()
	if len(m.apps) == 0 {
		t.Skip("no steam libraries found")
	}
	// menu -> Move (cursor 0, enter)
	m = keyPress(m, "enter")
	if m.screen != scPick || m.mode != "move" {
		t.Fatalf("expected pick/move, got screen=%d mode=%s", m.screen, m.mode)
	}
	// filter narrows
	m = keyPress(m, "w")
	m = keyPress(m, "a")
	m = keyPress(m, "r")
	if len(m.filtered) == 0 {
		t.Fatalf("filter 'war' matched nothing")
	}
	for _, i := range m.filtered {
		if !strings.Contains(strings.ToLower(m.apps[i].Name), "war") {
			t.Fatalf("filter leak: %s", m.apps[i].Name)
		}
	}
	// select first, confirm -> target screen (move)
	m = keyPress(m, " ")
	if m.selCount() != 1 {
		t.Fatalf("expected 1 selected, got %d", m.selCount())
	}
	m = keyPress(m, "enter")
	if m.screen != scTarget {
		t.Fatalf("expected target screen, got %d", m.screen)
	}
	m = keyPress(m, "enter")
	if m.screen != scConfirm {
		t.Fatalf("expected confirm screen, got %d", m.screen)
	}
	v := m.viewConfirm()
	if !strings.Contains(v, "Move 1 game(s)") {
		t.Fatalf("confirm view missing count:\n%s", v)
	}
}

func TestUninstallFlowSkipsTarget(t *testing.T) {
	m := testModel()
	if len(m.apps) == 0 {
		t.Skip("no steam libraries found")
	}
	m = keyPress(m, "down") // Uninstall
	m = keyPress(m, "enter")
	if m.mode != "uninstall" || m.screen != scPick {
		t.Fatalf("got mode=%s screen=%d", m.mode, m.screen)
	}
	m = keyPress(m, " ")
	m = keyPress(m, "down")
	m = keyPress(m, " ")
	m = keyPress(m, "enter")
	if m.screen != scConfirm {
		t.Fatalf("uninstall should go straight to confirm, got %d", m.screen)
	}
}

func TestConfirmStartsRunDry(t *testing.T) {
	m := testModel()
	if len(m.apps) == 0 {
		t.Skip("no steam libraries found")
	}
	m.opts.DryRun = true
	m.mode = "uninstall"
	m.toggle(0)
	m.screen = scConfirm
	m = keyPress(m, "y")
	if m.screen != scRun {
		t.Fatalf("expected run screen, got %d", m.screen)
	}
	if len(m.queue) != 1 {
		t.Fatalf("expected queue len 1, got %d", len(m.queue))
	}
}

func TestListAndFixViews(t *testing.T) {
	m := testModel()
	if len(m.apps) == 0 {
		t.Skip("no steam libraries found")
	}
	m.screen = scList
	if v := m.View(); !strings.Contains(v, "Installed games") {
		t.Fatalf("list view broken:\n%s", v)
	}
	m.inspectFix()
	m.screen = scFix
	if v := m.View(); !strings.Contains(v, "Fix libraries") {
		t.Fatalf("fix view broken:\n%s", v)
	}
}

func TestRealDataSanity(t *testing.T) {
	o := defaultOptions()
	apps := scanApps(o)
	if len(apps) == 0 {
		t.Skip("no steam libraries found")
	}
	found := false
	for _, a := range apps {
		if a.AppID == "230410" && a.Name == "Warframe" && a.ShortLib == "Home" && a.HasPrefix {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Warframe/Home/prefix in scan (%d apps)", len(apps))
	}
}
