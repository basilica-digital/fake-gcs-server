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

const signedAlgoQuery = "X-Goog-Algorithm=GOOG4-RSA-SHA256"

func startFSSignedServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	server, err := NewServerWithOptions(Options{
		Scheme:      "http",
		PublicHost:  "127.0.0.1",
		StorageRoot: dir,
		ExternalURL: "",
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

			startURL := server.URL() + "/" + bucketName + "/" + objectName
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
			if !strings.Contains(location, "uploadType=resumable") {
				t.Errorf("Location %q should be the JSON resumable upload URL", location)
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

			startURL := server.URL() + "/" + bucketName + "/" + objectName
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

			startURL := server.URL() + "/" + bucketName + "/" + objectName
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
	startURL := server.URL() + "/" + bucketName + "/missing.tar"
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
