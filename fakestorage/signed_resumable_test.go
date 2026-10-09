// Copyright 2017 Francisco Souza. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fakestorage

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	signedAlgoQuery   = "X-Goog-Algorithm=GOOG4-RSA-SHA256"
	signedExternalURL = "http://gcs.example.test:4443"
)

func startFSSignedServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	server, err := NewServerWithOptions(Options{
		Scheme:      "http",
		PublicHost:  "127.0.0.1",
		StorageRoot: dir,
		ExternalURL: signedExternalURL,
	})
	if err != nil {
		t.Fatalf("could not start server: %v", err)
	}
	t.Cleanup(server.Stop)
	return server, dir
}

func signedResumableStart(t *testing.T, client *http.Client, url, generationMatch string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-goog-resumable", "start")
	req.Header.Set("Content-Length", "0")
	if generationMatch != "" {
		req.Header.Set("x-goog-if-generation-match", generationMatch)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func putLocationChunk(t *testing.T, client *http.Client, location, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, location, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Range", "bytes 0-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSignedURLResumableStartCreatesObjectOnFilesystem(t *testing.T) {
	for _, withAlgo := range []bool{false, true} {
		name := "no-algo"
		if withAlgo {
			name = "goog4-algo-query"
		}
		t.Run(name, func(t *testing.T) {
			server, root := startFSSignedServer(t)
			const bucketName = "session-bucket"
			const objectName = "apps/abc/session.tar"
			server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
			client := server.HTTPClient()

			startURL := server.ts.URL + "/" + bucketName + "/" + objectName
			if withAlgo {
				startURL += "?" + signedAlgoQuery
			}
			initResp := signedResumableStart(t, client, startURL, "0")
			body, _ := io.ReadAll(initResp.Body)
			initResp.Body.Close()
			if initResp.StatusCode != http.StatusOK {
				t.Fatalf("signed resumable start: want 200, got %d (%s)", initResp.StatusCode, body)
			}
			location := initResp.Header.Get("Location")
			if location == "" {
				t.Fatal("signed resumable start: missing Location")
			}
			if !strings.HasPrefix(location, signedExternalURL+"/") {
				t.Errorf("Location %q should use -external-url %s", location, signedExternalURL)
			}
			if !strings.Contains(location, "uploadType=resumable") {
				t.Errorf("Location %q should be the JSON resumable upload URL", location)
			}

			_, err := server.GetObject(bucketName, objectName)
			if err == nil {
				t.Fatal("object must be absent after start and before chunk")
			}

			chunkResp := putLocationChunk(t, client, location, "archive-bytes")
			_, _ = io.Copy(io.Discard, chunkResp.Body)
			chunkResp.Body.Close()
			if chunkResp.StatusCode != http.StatusOK {
				t.Fatalf("chunk PUT: want 200, got %d", chunkResp.StatusCode)
			}
			obj, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			gotGen := chunkResp.Header.Get("x-goog-generation")
			wantGen := strconv.FormatInt(obj.Generation, 10)
			if gotGen != wantGen {
				t.Fatalf("finalize x-goog-generation: want %s, got %q", wantGen, gotGen)
			}
			if string(obj.Content) != "archive-bytes" {
				t.Errorf("object content: want %q, got %q", "archive-bytes", string(obj.Content))
			}
			if obj.Name != objectName {
				t.Errorf("object name: want %q, got %q", objectName, obj.Name)
			}

			bucketDir := filepath.Join(root, bucketName)
			info, err := os.Stat(bucketDir)
			if err != nil {
				t.Fatalf("bucket path %s: %v", bucketDir, err)
			}
			if !info.IsDir() {
				t.Fatalf("bucket path %s is not a directory", bucketDir)
			}
		})
	}
}

func TestSignedURLResumableStartGenerationZeroWhenObjectExists(t *testing.T) {
	for _, withAlgo := range []bool{false, true} {
		name := "no-algo"
		if withAlgo {
			name = "goog4-algo-query"
		}
		t.Run(name, func(t *testing.T) {
			server, _ := startFSSignedServer(t)
			const bucketName = "session-bucket"
			const objectName = "apps/abc/session.tar"
			const original = "original-bytes"
			server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
			server.CreateObject(Object{
				ObjectAttrs: ObjectAttrs{BucketName: bucketName, Name: objectName},
				Content:     []byte(original),
			})
			live, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			client := server.HTTPClient()

			startURL := server.ts.URL + "/" + bucketName + "/" + objectName
			if withAlgo {
				startURL += "?" + signedAlgoQuery
			}
			initResp := signedResumableStart(t, client, startURL, "0")
			_, _ = io.Copy(io.Discard, initResp.Body)
			initResp.Body.Close()
			if initResp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("start with match 0 on existing object: want 412, got %d", initResp.StatusCode)
			}
			gotGen := initResp.Header.Get("x-goog-generation")
			wantGen := strconv.FormatInt(live.Generation, 10)
			if gotGen != wantGen {
				t.Errorf("x-goog-generation: want %s, got %q", wantGen, gotGen)
			}
			if loc := initResp.Header.Get("Location"); loc != "" {
				t.Errorf("412 start must not create a session Location, got %q", loc)
			}

			after, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			if string(after.Content) != original {
				t.Errorf("object bytes changed after 412: want %q, got %q", original, string(after.Content))
			}
			if after.Generation != live.Generation {
				t.Errorf("generation changed after 412: want %d, got %d", live.Generation, after.Generation)
			}
		})
	}
}

func TestSignedURLResumableStartLiveGenerationThenStaleStart(t *testing.T) {
	for _, withAlgo := range []bool{false, true} {
		name := "no-algo"
		if withAlgo {
			name = "goog4-algo-query"
		}
		t.Run(name, func(t *testing.T) {
			server, _ := startFSSignedServer(t)
			const bucketName = "session-bucket"
			const objectName = "apps/abc/session.tar"
			server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
			server.CreateObject(Object{
				ObjectAttrs: ObjectAttrs{BucketName: bucketName, Name: objectName},
				Content:     []byte("v1"),
			})
			first, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			oldGen := first.Generation
			client := server.HTTPClient()

			startURL := server.ts.URL + "/" + bucketName + "/" + objectName
			if withAlgo {
				startURL += "?" + signedAlgoQuery
			}
			initResp := signedResumableStart(t, client, startURL, strconv.FormatInt(oldGen, 10))
			body, _ := io.ReadAll(initResp.Body)
			initResp.Body.Close()
			if initResp.StatusCode != http.StatusOK {
				t.Fatalf("start with live generation: want 200, got %d (%s)", initResp.StatusCode, body)
			}
			location := initResp.Header.Get("Location")
			if location == "" {
				t.Fatal("missing Location")
			}
			chunkResp := putLocationChunk(t, client, location, "v2")
			_, _ = io.Copy(io.Discard, chunkResp.Body)
			chunkResp.Body.Close()
			if chunkResp.StatusCode != http.StatusOK {
				t.Fatalf("chunk PUT: want 200, got %d", chunkResp.StatusCode)
			}

			updated, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			if string(updated.Content) != "v2" {
				t.Errorf("content after commit: want %q, got %q", "v2", string(updated.Content))
			}
			if updated.Generation == oldGen {
				t.Fatal("generation did not change after commit")
			}

			stale := signedResumableStart(t, client, startURL, strconv.FormatInt(oldGen, 10))
			_, _ = io.Copy(io.Discard, stale.Body)
			stale.Body.Close()
			if stale.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("start with stale generation: want 412, got %d", stale.StatusCode)
			}
			gotGen := stale.Header.Get("x-goog-generation")
			wantGen := strconv.FormatInt(updated.Generation, 10)
			if gotGen != wantGen {
				t.Errorf("x-goog-generation after stale start: want %s, got %q", wantGen, gotGen)
			}
			still, err := server.GetObject(bucketName, objectName)
			if err != nil {
				t.Fatal(err)
			}
			if string(still.Content) != "v2" {
				t.Errorf("stale start mutated object: got %q", string(still.Content))
			}
		})
	}
}

func TestSignedURLResumableStartOmitsGenerationWhenObjectAbsent(t *testing.T) {
	server, _ := startFSSignedServer(t)
	const bucketName = "session-bucket"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	startURL := server.ts.URL + "/" + bucketName + "/missing.tar"
	// Match a non-zero generation while the object is absent.
	initResp := signedResumableStart(t, client, startURL, "99")
	_, _ = io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d", initResp.StatusCode)
	}
	if got := initResp.Header.Get("x-goog-generation"); got != "" {
		t.Errorf("absent object must omit x-goog-generation, got %q", got)
	}
}

func TestWrapUploadPreconditionsReadsGoogHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/b/o", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-goog-if-generation-match", "7")
	req.Header.Set("x-goog-if-generation-not-match", "8")
	req.Header.Set("x-goog-if-metageneration-match", "1")
	req.Header.Set("x-goog-if-metageneration-not-match", "2")
	s := &Server{}
	c, err := s.wrapUploadPreconditions(req, "b", "o")
	if err != nil {
		t.Fatal(err)
	}
	if c.ifGenerationMatch == nil || *c.ifGenerationMatch != 7 {
		t.Errorf("ifGenerationMatch: got %v", c.ifGenerationMatch)
	}
	if c.ifGenerationNotMatch == nil || *c.ifGenerationNotMatch != 8 {
		t.Errorf("ifGenerationNotMatch: got %v", c.ifGenerationNotMatch)
	}
	if c.ifMetagenerationMatch == nil || *c.ifMetagenerationMatch != 1 {
		t.Errorf("ifMetagenerationMatch: got %v", c.ifMetagenerationMatch)
	}
	if c.ifMetagenerationNotMatch == nil || *c.ifMetagenerationNotMatch != 2 {
		t.Errorf("ifMetagenerationNotMatch: got %v", c.ifMetagenerationNotMatch)
	}
	if !c.ConditionsMet(7) {
		t.Error("generation 7 should match")
	}
	if c.ConditionsMet(9) {
		t.Error("generation 9 should not match")
	}
	if !c.metaConditionsMet(true) {
		t.Error("live object metageneration 1 should match")
	}
	if c.metaConditionsMet(false) {
		t.Error("absent object metageneration should fail match 1")
	}
}

func TestSignedURLResumableStartQueryPreconditionsAndContentType(t *testing.T) {
	server, _ := startFSSignedServer(t)
	const bucketName = "session-bucket"
	const objectName = "apps/abc/session.tar"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	startURL := server.ts.URL + "/" + bucketName + "/" + objectName + "?ifGenerationMatch=0"
	req, err := http.NewRequest(http.MethodPost, startURL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-goog-resumable", "start")
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("Cache-Control", "private")
	initResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("query ifGenerationMatch=0 start: want 200, got %d", initResp.StatusCode)
	}
	chunkResp := putLocationChunk(t, client, initResp.Header.Get("Location"), "tar-bytes")
	_, _ = io.Copy(io.Discard, chunkResp.Body)
	chunkResp.Body.Close()
	if chunkResp.StatusCode != http.StatusOK {
		t.Fatalf("chunk PUT: want 200, got %d", chunkResp.StatusCode)
	}
	obj, err := server.GetObject(bucketName, objectName)
	if err != nil {
		t.Fatal(err)
	}
	if obj.ContentType != "application/x-tar" {
		t.Errorf("content type: want application/x-tar, got %q", obj.ContentType)
	}
	if obj.CacheControl != "private" {
		t.Errorf("cache control: want private, got %q", obj.CacheControl)
	}

	// Query ifGenerationMatch=0 must 412 when the object now exists.
	failURL := server.ts.URL + "/" + bucketName + "/" + objectName + "?ifGenerationMatch=0"
	failReq, err := http.NewRequest(http.MethodPost, failURL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	failReq.Header.Set("x-goog-resumable", "start")
	failResp, err := client.Do(failReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, failResp.Body)
	failResp.Body.Close()
	if failResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("query ifGenerationMatch=0 on existing object: want 412, got %d", failResp.StatusCode)
	}

	// Query wins over a matching header: query 0 vs header live generation is 412.
	conflictURL := server.ts.URL + "/" + bucketName + "/" + objectName + "?ifGenerationMatch=0"
	conflictReq, err := http.NewRequest(http.MethodPost, conflictURL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	conflictReq.Header.Set("x-goog-resumable", "start")
	conflictReq.Header.Set("x-goog-if-generation-match", strconv.FormatInt(obj.Generation, 10))
	conflictResp, err := client.Do(conflictReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, conflictResp.Body)
	conflictResp.Body.Close()
	if conflictResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("query 0 overrides matching header: want 412, got %d", conflictResp.StatusCode)
	}
}

func TestSignedURLResumableFinalizeEnforcesStoredPreconditions(t *testing.T) {
	server, _ := startFSSignedServer(t)
	const bucketName = "session-bucket"
	const objectName = "apps/abc/session.tar"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	startURL := server.ts.URL + "/" + bucketName + "/" + objectName
	initResp := signedResumableStart(t, client, startURL, "0")
	_, _ = io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("start: want 200, got %d", initResp.StatusCode)
	}
	location := initResp.Header.Get("Location")
	server.CreateObject(Object{
		ObjectAttrs: ObjectAttrs{BucketName: bucketName, Name: objectName},
		Content:     []byte("racer"),
	})
	live, err := server.GetObject(bucketName, objectName)
	if err != nil {
		t.Fatal(err)
	}
	chunkResp := putLocationChunk(t, client, location, "should-not-land")
	_, _ = io.Copy(io.Discard, chunkResp.Body)
	chunkResp.Body.Close()
	if chunkResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("finalize after racer: want 412, got %d", chunkResp.StatusCode)
	}
	gotGen := chunkResp.Header.Get("x-goog-generation")
	wantGen := strconv.FormatInt(live.Generation, 10)
	if gotGen != wantGen {
		t.Errorf("finalize 412 x-goog-generation: want %s, got %q", wantGen, gotGen)
	}
	after, err := server.GetObject(bucketName, objectName)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Content) != "racer" {
		t.Errorf("racer bytes changed: got %q", string(after.Content))
	}
}

func TestSignedURLResumableFinalizeEnforcesMetageneration(t *testing.T) {
	server, _ := startFSSignedServer(t)
	const bucketName = "session-bucket"
	const objectName = "apps/abc/session.tar"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	startURL := server.ts.URL + "/" + bucketName + "/" + objectName
	req, err := http.NewRequest(http.MethodPost, startURL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-goog-resumable", "start")
	req.Header.Set("x-goog-if-metageneration-match", "0")
	initResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("start with metageneration match 0: want 200, got %d", initResp.StatusCode)
	}
	server.CreateObject(Object{
		ObjectAttrs: ObjectAttrs{BucketName: bucketName, Name: objectName},
		Content:     []byte("created-in-between"),
	})
	chunkResp := putLocationChunk(t, client, initResp.Header.Get("Location"), "overwrite")
	_, _ = io.Copy(io.Discard, chunkResp.Body)
	chunkResp.Body.Close()
	if chunkResp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("finalize after object appeared: want 412, got %d", chunkResp.StatusCode)
	}
	after, err := server.GetObject(bucketName, objectName)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Content) != "created-in-between" {
		t.Errorf("object overwritten: got %q", string(after.Content))
	}
}

func TestSignedURLResumableStartRejectsPathEscape(t *testing.T) {
	server, root := startFSSignedServer(t)
	const bucketName = "session-bucket"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	parent := filepath.Dir(root)
	before, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}

	startURL := server.ts.URL + "/" + bucketName + "/%2e%2e/escaped.txt"
	initResp := signedResumableStart(t, client, startURL, "0")
	_, _ = io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("path-escaping object name: want 400, got %d", initResp.StatusCode)
	}
	if loc := initResp.Header.Get("Location"); loc != "" {
		t.Fatalf("rejected start must not return Location, got %q", loc)
	}
	outside := filepath.Join(parent, "escaped.txt")
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("wrote file outside storage root: %s", outside)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); err == nil {
		t.Fatal("wrote escaped object at storage root")
	}

	for _, name := range []string{"foo/..", "foo/../bar", "."} {
		u := server.ts.URL + "/" + bucketName + "/" + name
		resp := signedResumableStart(t, client, u, "0")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("object name %q: want 400, got %d", name, resp.StatusCode)
		}
	}

	dotdot := signedResumableStart(t, client, server.ts.URL+"/%2e%2e/pwned.txt", "0")
	_, _ = io.Copy(io.Discard, dotdot.Body)
	dotdot.Body.Close()
	if dotdot.StatusCode != http.StatusBadRequest {
		t.Fatalf("bucket .. start: want 400, got %d", dotdot.StatusCode)
	}
	if loc := dotdot.Header.Get("Location"); loc != "" {
		t.Fatalf("bucket .. start must not return Location, got %q", loc)
	}
	if _, err := os.Stat(filepath.Join(parent, "pwned.txt")); err == nil {
		t.Fatal("wrote pwned.txt outside storage root")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(before) {
		t.Fatalf("parent of StorageRoot changed: before %d entries, after %d", len(before), len(entries))
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "bucketMetadata") {
			t.Fatalf("unexpected metadata outside root: %s", e.Name())
		}
	}
}

func TestSignedURLResumableLocationQueryEscapesObjectName(t *testing.T) {
	server, _ := startFSSignedServer(t)
	const bucketName = "session-bucket"
	const objectName = "victim&upload_id=other"
	server.CreateBucketWithOpts(CreateBucketOpts{Name: bucketName})
	client := server.HTTPClient()
	startURL := server.ts.URL + "/" + bucketName + "/" + objectName
	initResp := signedResumableStart(t, client, startURL, "0")
	body, _ := io.ReadAll(initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("start: want 200, got %d (%s)", initResp.StatusCode, body)
	}
	location := initResp.Header.Get("Location")
	if strings.Contains(location, "victim&upload_id=") {
		t.Fatalf("Location left & unescaped: %s", location)
	}
	if !strings.Contains(location, "name=victim%26upload_id%3Dother") && !strings.Contains(location, "name=victim%26upload_id%3dother") {
		t.Errorf("Location should query-escape object name, got %s", location)
	}
	chunkResp := putLocationChunk(t, client, location, "safe")
	_, _ = io.Copy(io.Discard, chunkResp.Body)
	chunkResp.Body.Close()
	if chunkResp.StatusCode != http.StatusOK {
		t.Fatalf("chunk PUT: want 200, got %d", chunkResp.StatusCode)
	}
	obj, err := server.GetObject(bucketName, objectName)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj.Content) != "safe" {
		t.Errorf("content: want safe, got %q", string(obj.Content))
	}
}
