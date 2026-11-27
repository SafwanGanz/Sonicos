package owner

import "testing"

func TestIsOwner(t *testing.T) {
	Set([]string{"917306771605@s.whatsapp.net", " 0987654321 "})
	if !IsOwner("917306771605:12@s.whatsapp.net") {
		t.Fatal("device-suffixed JID must match")
	}
	if !IsOwner("917306771605") {
		t.Fatal("bare number must match")
	}
	if !IsOwner("0987654321") {
		t.Fatal("second owner must match")
	}
	if IsOwner("1111111111") {
		t.Fatal("unauthorized number must not match")
	}
	if IsOwner("") {
		t.Fatal("empty sender must not match")
	}
}

func TestConfiguredAndList(t *testing.T) {
	Set(nil)
	if Configured() {
		t.Fatal("empty owner list must report not configured")
	}
	Set([]string{"42"})
	if !Configured() || len(List()) != 1 || List()[0] != "42" {
		t.Fatalf("list = %v", List())
	}
}

func TestParseList(t *testing.T) {
	got := ParseList(" 123 , 456 ,, ")
	if len(got) != 2 || got[0] != "123" || got[1] != "456" {
		t.Fatalf("ParseList = %v", got)
	}
	if ParseList("   ") != nil {
		t.Fatal("blank must be nil")
	}
}