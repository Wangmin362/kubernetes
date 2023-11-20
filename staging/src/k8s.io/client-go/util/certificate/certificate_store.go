/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certificate

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
)

const (
	keyExtension  = ".key"
	certExtension = ".crt"
	pemExtension  = ".pem"
	currentPair   = "current"
	updatedPair   = "updated"
)

type fileStore struct {
	pairNamePrefix string
	certDirectory  string
	keyDirectory   string
	certFile       string
	keyFile        string
}

// FileStore is a store that provides certificate retrieval as well as
// the path on disk of the current PEM.
type FileStore interface {
	Store
	// CurrentPath returns the path on disk of the current certificate/key
	// pair encoded as PEM files.
	// 获取当前证书保存路径，该文件包含了证书、私钥
	CurrentPath() string
}

// NewFileStore returns a concrete implementation of a Store that is based on
// storing the cert/key pairs in a single file per pair on disk in the
// designated directory. When starting up it will look for the currently
// selected cert/key pair in:
//
// 1. ${certDirectory}/${pairNamePrefix}-current.pem - both cert and key are in the same file.
// 2. ${certFile}, ${keyFile}
// 3. ${certDirectory}/${pairNamePrefix}.crt, ${keyDirectory}/${pairNamePrefix}.key
//
// The first one found will be used. If rotation is enabled, future cert/key
// updates will be written to the ${certDirectory} directory and
// ${certDirectory}/${pairNamePrefix}-current.pem will be created as a soft
// link to the currently selected cert/key pair.
// 用于保存证书
func NewFileStore(
	pairNamePrefix string, // kubelet-client
	certDirectory string, // 证书目录，一般默认为/var/lib/kubelet/pki
	keyDirectory string, // 私钥目录，一般默认为/var/lib/kubelet/pki
	certFile string, // 证书路径， /var/lib/kubelet/pki/kubelet-client-current.pem
	keyFile string, // 私钥路径， /var/lib/kubelet/pki/kubelet-client-current.pem
) (FileStore, error) {

	s := fileStore{
		pairNamePrefix: pairNamePrefix, // kubelet-client
		certDirectory:  certDirectory,
		keyDirectory:   keyDirectory,
		certFile:       certFile,
		keyFile:        keyFile,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return &s, nil
}

// CurrentPath returns the path to the current version of these certificates.
// 路径为：/var/lib/kubelet/pki/kubelet-client-current.pem
func (s *fileStore) CurrentPath() string {
	return filepath.Join(s.certDirectory, s.filename(currentPair))
}

// recover checks if there is a certificate rotation that was interrupted while
// progress, and if so, attempts to recover to a good state.
func (s *fileStore) recover() error {
	// If the 'current' file doesn't exist, continue on with the recovery process.
	// 路径为：/var/lib/kubelet/pki/kubelet-client-current.pem
	currentPath := filepath.Join(s.certDirectory, s.filename(currentPair))
	if exists, err := fileExists(currentPath); err != nil {
		return err
	} else if exists {
		// 如果证书存在，直接返回
		return nil
	}

	// If the 'updated' file exists, and it is a symbolic link, continue on
	// with the recovery process.
	// 路径为：/var/lib/kubelet/pki/kubelet-client-update.pem
	updatedPath := filepath.Join(s.certDirectory, s.filename(updatedPair))
	if fi, err := os.Lstat(updatedPath); err != nil {
		// 如果此文件不存在，直接返回
		if os.IsNotExist(err) {
			return nil
		}
		return err
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		// /var/lib/kubelet/pki/kubelet-client-update.pem文件必须是符号连接文件
		return fmt.Errorf("expected %q to be a symlink but it is a file", updatedPath)
	}

	// Move the 'updated' symlink to 'current'.
	// 否则直接把/var/lib/kubelet/pki/kubelet-client-update.pem重命名为/var/lib/kubelet/pki/kubelet-client-current.pem
	if err := os.Rename(updatedPath, currentPath); err != nil {
		return fmt.Errorf("unable to rename %q to %q: %v", updatedPath, currentPath, err)
	}
	return nil
}

func (s *fileStore) Current() (*tls.Certificate, error) {
	// /var/lib/kubelet/pki/kubelet-client-current.pem
	pairFile := filepath.Join(s.certDirectory, s.filename(currentPair))
	if pairFileExists, err := fileExists(pairFile); err != nil {
		return nil, err
	} else if pairFileExists {
		klog.Infof("Loading cert/key pair from %q.", pairFile)
		// 加载/var/lib/kubelet/pki/kubelet-client-current.pem证书
		return loadFile(pairFile)
	}

	certFileExists, err := fileExists(s.certFile)
	if err != nil {
		return nil, err
	}
	keyFileExists, err := fileExists(s.keyFile)
	if err != nil {
		return nil, err
	}
	if certFileExists && keyFileExists {
		klog.Infof("Loading cert/key pair from (%q, %q).", s.certFile, s.keyFile)
		return loadX509KeyPair(s.certFile, s.keyFile)
	}

	// /var/lib/kubelet/pki/kubelet-client.crt
	c := filepath.Join(s.certDirectory, s.pairNamePrefix+certExtension)
	// /var/lib/kubelet/pki/kubelet-client.key
	k := filepath.Join(s.keyDirectory, s.pairNamePrefix+keyExtension)
	certFileExists, err = fileExists(c)
	if err != nil {
		return nil, err
	}
	keyFileExists, err = fileExists(k)
	if err != nil {
		return nil, err
	}
	if certFileExists && keyFileExists {
		klog.Infof("Loading cert/key pair from (%q, %q).", c, k)
		return loadX509KeyPair(c, k)
	}

	noKeyErr := NoCertKeyError(
		fmt.Sprintf("no cert/key files read at %q, (%q, %q) or (%q, %q)",
			pairFile,
			s.certFile,
			s.keyFile,
			s.certDirectory,
			s.keyDirectory))
	return nil, &noKeyErr
}

func loadFile(pairFile string) (*tls.Certificate, error) {
	// LoadX509KeyPair knows how to parse combined cert and private key from
	// the same file.
	cert, err := tls.LoadX509KeyPair(pairFile, pairFile)
	if err != nil {
		return nil, fmt.Errorf("could not convert data from %q into cert/key pair: %v", pairFile, err)
	}
	certs, err := x509.ParseCertificates(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificate data: %v", err)
	}
	cert.Leaf = certs[0]
	return &cert, nil
}

func (s *fileStore) Update(certData, keyData []byte) (*tls.Certificate, error) {
	ts := time.Now().Format("2006-01-02-15-04-05")
	// kubelet-client-<date>.pem
	pemFilename := s.filename(ts)

	// 创建/var/lib/kubelet/pki目录
	if err := os.MkdirAll(s.certDirectory, 0755); err != nil {
		return nil, fmt.Errorf("could not create directory %q to store certificates: %v", s.certDirectory, err)
	}
	// /var/lib/kubelet/pki/kubelet-client-<date>.pem
	certPath := filepath.Join(s.certDirectory, pemFilename)

	// 创建文件
	f, err := os.OpenFile(certPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open %q: %v", certPath, err)
	}
	defer f.Close()

	// First cert is leaf, remainder are intermediates
	certs, err := certutil.ParseCertsPEM(certData)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate data: %v", err)
	}
	// 写入证书
	for _, c := range certs {
		pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}

	// 写入私钥
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return nil, fmt.Errorf("invalid key data")
	}
	pem.Encode(f, keyBlock)

	// 加载证书
	cert, err := loadFile(certPath)
	if err != nil {
		return nil, err
	}

	// 更新连接文件
	if err := s.updateSymlink(certPath); err != nil {
		return nil, err
	}
	return cert, nil
}

// updateSymLink updates the current symlink to point to the file that is
// passed it. It will fail if there is a non-symlink file exists where the
// symlink is expected to be.
func (s *fileStore) updateSymlink(filename string) error {
	// If the 'current' file either doesn't exist, or is already a symlink,
	// proceed. Otherwise, this is an unrecoverable error.
	// /var/lib/kubelet/pki/kubelet-client-current.pem
	currentPath := filepath.Join(s.certDirectory, s.filename(currentPair))
	currentPathExists := false
	if fi, err := os.Lstat(currentPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		return fmt.Errorf("expected %q to be a symlink but it is a file", currentPath)
	} else {
		currentPathExists = true
	}

	// If the 'updated' file doesn't exist, proceed. If it exists but it is a
	// symlink, delete it.  Otherwise, this is an unrecoverable error.
	updatedPath := filepath.Join(s.certDirectory, s.filename(updatedPair))
	if fi, err := os.Lstat(updatedPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		return fmt.Errorf("expected %q to be a symlink but it is a file", updatedPath)
	} else {
		if err := os.Remove(updatedPath); err != nil {
			return fmt.Errorf("unable to remove %q: %v", updatedPath, err)
		}
	}

	// Check that the new cert/key pair file exists to avoid rotating to an
	// invalid cert/key.
	if filenameExists, err := fileExists(filename); err != nil {
		return err
	} else if !filenameExists {
		return fmt.Errorf("file %q does not exist so it can not be used as the currently selected cert/key", filename)
	}

	// Ensure the source path is absolute to ensure the symlink target is
	// correct when certDirectory is a relative path.
	filename, err := filepath.Abs(filename)
	if err != nil {
		return err
	}

	// Create the 'updated' symlink pointing to the requested file name.
	if err := os.Symlink(filename, updatedPath); err != nil {
		return fmt.Errorf("unable to create a symlink from %q to %q: %v", updatedPath, filename, err)
	}

	// Replace the 'current' symlink.
	if currentPathExists {
		if err := os.Remove(currentPath); err != nil {
			return fmt.Errorf("unable to remove %q: %v", currentPath, err)
		}
	}
	if err := os.Rename(updatedPath, currentPath); err != nil {
		return fmt.Errorf("unable to rename %q to %q: %v", updatedPath, currentPath, err)
	}
	return nil
}

func (s *fileStore) filename(qualifier string) string {
	// kubelet-client-<qualifier>.pem
	return s.pairNamePrefix + "-" + qualifier + pemExtension
}

func loadX509KeyPair(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	certs, err := x509.ParseCertificates(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificate data: %v", err)
	}
	cert.Leaf = certs[0]
	return &cert, nil
}

// FileExists checks if specified file exists.
func fileExists(filename string) (bool, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}
