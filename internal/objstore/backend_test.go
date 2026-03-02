// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package objstore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/nishisan-dev/n-backup/internal/objstore"
)

func TestMockBackend_InterfaceContract(t *testing.T) {
	if err := objstore.TestableBackendInterface(); err != nil {
		t.Fatalf("interface contract broken: %v", err)
	}
}

func TestMockBackend_UploadAndList(t *testing.T) {
	m := objstore.NewMockBackend()
	ctx := context.Background()

	if err := m.Upload(ctx, "/dev/null", "prefix/file1.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := m.Upload(ctx, "/dev/null", "prefix/file2.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := m.Upload(ctx, "/dev/null", "other/file3.tar.gz"); err != nil {
		t.Fatal(err)
	}

	// List com prefixo
	objs, err := m.List(ctx, "prefix/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects with prefix 'prefix/', got %d", len(objs))
	}
	if objs[0].Key != "prefix/file1.tar.gz" {
		t.Errorf("expected first key 'prefix/file1.tar.gz', got %q", objs[0].Key)
	}

	if len(m.UploadCalls) != 3 {
		t.Errorf("expected 3 upload calls, got %d", len(m.UploadCalls))
	}
}

func TestMockBackend_Delete(t *testing.T) {
	m := objstore.NewMockBackend()
	ctx := context.Background()

	m.Seed("a/1.tar.gz", "a/2.tar.gz", "a/3.tar.gz")

	if err := m.Delete(ctx, "a/2.tar.gz"); err != nil {
		t.Fatal(err)
	}

	keys := m.ObjectKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 objects after delete, got %d", len(keys))
	}
	if keys[0] != "a/1.tar.gz" || keys[1] != "a/3.tar.gz" {
		t.Errorf("unexpected keys: %v", keys)
	}

	if len(m.DeleteCalls) != 1 || m.DeleteCalls[0] != "a/2.tar.gz" {
		t.Errorf("expected 1 delete call for a/2.tar.gz, got %v", m.DeleteCalls)
	}
}

func TestMockBackend_ErrorInjection(t *testing.T) {
	m := objstore.NewMockBackend()
	ctx := context.Background()

	m.UploadErr = fmt.Errorf("network timeout")
	if err := m.Upload(ctx, "/dev/null", "test/file.tar.gz"); err == nil {
		t.Fatal("expected upload error")
	}

	m.UploadErr = nil
	m.DeleteErr = fmt.Errorf("access denied")
	if err := m.Delete(ctx, "test/file.tar.gz"); err == nil {
		t.Fatal("expected delete error")
	}

	m.DeleteErr = nil
	m.ListErr = fmt.Errorf("bucket not found")
	if _, err := m.List(ctx, "test/"); err == nil {
		t.Fatal("expected list error")
	}
}

func TestMockBackend_Seed(t *testing.T) {
	m := objstore.NewMockBackend()
	m.Seed("x/a.tar.gz", "x/b.tar.gz", "y/c.tar.gz")

	keys := m.ObjectKeys()
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}

	objs, err := m.List(context.Background(), "x/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects with prefix x/, got %d", len(objs))
	}
}
