package store

import (
	"context"
	"errors"
	"testing"

	"iot-server/internal/media"
)

type roomLog struct {
	deleted []string
	removed []string
}

type scriptProvider struct {
	media.TestProvider
	log *roomLog
}

func (p scriptProvider) DeleteRoom(_ context.Context, resourceID string) error {
	p.log.deleted = append(p.log.deleted, resourceID)
	return nil
}

func (p scriptProvider) RemoveParticipant(_ context.Context, resourceID, identity string) error {
	p.log.removed = append(p.log.removed, resourceID+" "+identity)
	return nil
}

func TestFreshRoomIsDeletedOnlyWhenUnused(t *testing.T) {
	if newRoomID(true, media.Room{ID: "pion-s", Created: true}) != "" {
		t.Fatal("an existing room was treated as new")
	}
	if newRoomID(false, media.Room{ID: "pion-s", Created: false}) != "" {
		t.Fatal("a reused room was treated as new")
	}
	if newRoomID(false, media.Room{ID: "pion-s", Provider: "pion", Created: true}) != "pion-s" {
		t.Fatal("a room created in this attempt was ignored")
	}

	log := &roomLog{}
	store := &Store{mediaProvider: scriptProvider{log: log}}
	if err := releaseUnusedRoom(context.Background(), store, "pion-s", errors.New("commit")); err == nil || len(log.deleted) != 1 || log.deleted[0] != "pion-s" {
		t.Fatal("uncommitted room was kept")
	}
	log.deleted = nil
	if err := releaseUnusedRoom(context.Background(), store, "pion-s", nil); err != nil || len(log.deleted) != 0 {
		t.Fatal("committed room was deleted")
	}
	if err := releaseUnusedRoom(context.Background(), store, "", errors.New("commit")); err == nil || len(log.deleted) != 0 {
		t.Fatal("reused room was deleted")
	}
}

func TestEndPathRemovesPeersBeforeDeletingRoom(t *testing.T) {
	log := &roomLog{}
	store := &Store{mediaProvider: scriptProvider{log: log}}
	store.releaseRoom(context.Background(), "pion-s", []string{"device:aabbccddeeff", "viewer:v1"}, true)
	if len(log.removed) != 2 || len(log.deleted) != 0 {
		t.Fatalf("shared room removed=%d deleted=%d", len(log.removed), len(log.deleted))
	}
	store.releaseRoom(context.Background(), "pion-s", []string{"viewer:v1"}, false)
	if len(log.deleted) != 1 || log.deleted[0] != "pion-s" {
		t.Fatal("unshared room was kept")
	}
}
