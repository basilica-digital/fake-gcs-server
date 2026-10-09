// Copyright 2018 Francisco Souza. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package backend provides the backends used by fake-gcs-server.
package backend

import (
	"path"
	"strings"

	"cloud.google.com/go/storage"
)

type Conditions interface {
	ConditionsMet(activeGeneration int64) bool
}

type NoConditions struct{}

func (NoConditions) ConditionsMet(int64) bool {
	return true
}

// Storage is the generic interface for implementing the backend storage of the
// server.
type Storage interface {
	CreateBucket(name string, bucketAttrs BucketAttrs) error
	ListBuckets() ([]Bucket, error)
	GetBucket(name string) (Bucket, error)
	UpdateBucket(name string, attrsToUpdate BucketAttrs) error
	UpdateBucketACL(name string, acl []storage.ACLRule) error
	DeleteBucket(name string) error
	CreateObject(obj StreamingObject, conditions Conditions) (StreamingObject, error)
	ListObjects(bucketName string, prefix string, versions bool) ([]ObjectAttrs, error)
	GetObject(bucketName, objectName string) (StreamingObject, error)
	GetObjectWithGeneration(bucketName, objectName string, generation int64) (StreamingObject, error)
	DeleteObject(bucketName, objectName string) error
	DeleteObjectWithGeneration(bucketName, objectName string, generation int64) error
	PatchObject(bucketName, objectName string, attrsToUpdate ObjectAttrs) (StreamingObject, error)
	UpdateObject(bucketName, objectName string, attrsToUpdate ObjectAttrs) (StreamingObject, error)
	ComposeObject(bucketName string, objectNames []string, destinationName string, metadata map[string]string, contentType string, contentEncoding string, contentDisposition string, contentLanguage string, cacheControl string, storageClass string, acl []storage.ACLRule) (StreamingObject, error)
	DeleteAllFiles() error
}

type Error string

func (e Error) Error() string { return string(e) }

const (
	BucketNotFound     = Error("bucket not found")
	BucketNotEmpty     = Error("bucket must be empty prior to deletion")
	PreConditionFailed = Error("Precondition failed")
	InvalidObjectName  = Error("invalid object name")
	InvalidBucketName  = Error("invalid bucket name")
)

// BucketNameInvalid is true for names that must not become a directory under
// the storage root (empty, ".", "..", or containing a path separator).
func BucketNameInvalid(name string) bool {
	if name == "" || name == "." || name == ".." {
		return true
	}
	return strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0)
}

// ObjectNameEscapesBucket is true when name would resolve outside the bucket
// directory, collapse to another name, or is empty/"."/"..".
func ObjectNameEscapesBucket(objectName string) bool {
	if objectName == "" || strings.ContainsRune(objectName, 0) {
		return true
	}
	cleaned := path.Clean(objectName)
	if path.IsAbs(cleaned) {
		return true
	}
	if cleaned == "." || cleaned == ".." {
		return true
	}
	if strings.HasPrefix(cleaned, "../") {
		return true
	}
	return cleaned != objectName
}
