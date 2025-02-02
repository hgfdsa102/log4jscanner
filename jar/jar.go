// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package jar implements JAR scanning capabilities for log4j.
package jar

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

const (
	maxZipDepth = 16
	maxZipSize  = 4 << 30 // 4GiB
)

var exts = map[string]bool{
	".jar":  true,
	".war":  true,
	".ear":  true,
	".zip":  true,
	".jmod": true,
}

// Report contains information about a scanned JAR.
type Report struct {
	// Vulnerable reports if a vulnerable version of the log4j is included in the
	// JAR and has been initialized.
	//
	// Note that this package considers the 2.15.0 versions vulnerable.
	Vulnerable bool

	// MainClass and Version are information taken from the MANIFEST.MF file.
	// Version indicates the version of JAR, NOT the log4j package.
	MainClass string
	Version   string
}

// Parse traverses a JAR file, attempting to detect any usages of vulnerable
// log4j versions.
func Parse(r fs.FS) (*Report, error) {
	var c checker
	if err := c.checkJAR(&zipFS{r}, 0, 0); err != nil {
		return nil, fmt.Errorf("failed to check JAR: %v", err)
	}
	return &Report{
		Vulnerable: c.bad(),
		MainClass:  c.mainClass,
		Version:    c.version,
	}, nil
}

// zipFS exists because of bugs hit in encoding/zip that causes reading "."
// to return "." as one of the entries. This in turn, makes fs.WalkDir()
// recurse infinitely.
//
// See: https://go.dev/issue/50179
type zipFS struct {
	fs.FS
}

// ReadDir overrides the buggy zip.Reader behavior and removes any DirEntry
// values that match the provided name.
func (z *zipFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(z.FS, name)
	if err != nil {
		return nil, err
	}

	j := 0
	for i := 0; i < len(entries); i++ {
		if entries[i].Name() == name {
			continue
		}
		entries[j] = entries[i]
		j++
	}
	entries = entries[:j]
	return entries, nil
}

type checker struct {
	// Does the JAR contain the JNDI lookup class?
	hasLookupClass bool
	// Does the JAR contain JndiManager with the old constructor, a
	// version that hasn't been fixed.
	hasOldJndiManagerConstructor bool
	// Does the jar contain a string that was added in 2.16 and whether we've checked for it yet
	seenJndiManagerClass   bool
	isAtLeastTwoDotSixteen bool

	mainClass string
	version   string
}

func (c *checker) done() bool {
	return c.bad() && c.mainClass != ""
}

func (c *checker) bad() bool {
	return (c.hasLookupClass && c.hasOldJndiManagerConstructor) || (c.hasLookupClass && c.seenJndiManagerClass && !c.isAtLeastTwoDotSixteen)
}

func (c *checker) checkJAR(r fs.FS, depth int, size int64) error {
	if depth > maxZipDepth {
		return fmt.Errorf("reached max zip depth of %d", maxZipDepth)
	}

	err := fs.WalkDir(r, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if c.done() {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		if strings.HasSuffix(p, ".class") {
			// Same logic as http://google3/security/tools/seam/cli/log4j_check.py
			if c.bad() {
				// Already determined that the content is bad, no
				// need to check more.
				return nil
			}

			f, err := r.Open(p)
			if err != nil {
				return fmt.Errorf("opening file %s: %v", p, err)
			}
			defer f.Close()

			info, err := f.Stat()
			if err != nil {
				return fmt.Errorf("stat file %s: %v", p, err)
			}
			var r io.Reader = f
			if fsize := info.Size(); fsize > 0 {
				if fsize+size > maxZipSize {
					return fmt.Errorf("reading %s would exceed memory limit: %v", p, err)
				}
				r = io.LimitReader(f, fsize)
			}

			content, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("reading file %s: %v", p, err)
			}
			if !c.hasLookupClass {
				if strings.Contains(p, "JndiLookup.class") {
					c.hasLookupClass = true
				}
			}
			if !c.hasOldJndiManagerConstructor {
				c.hasOldJndiManagerConstructor = strings.Contains(p, "JndiManager") && matchesLog4JYARARule(content)
			}
			if strings.Contains(p, "JndiManager.class") {
				c.seenJndiManagerClass = true
				c.isAtLeastTwoDotSixteen = matchesTwoSixteen(content)
			}
			return nil
		}
		if p == "META-INF/MANIFEST.MF" {
			mf, err := r.Open(p)
			if err != nil {
				return fmt.Errorf("opening manifest file %s: %v", p, err)
			}
			defer mf.Close()
			s := bufio.NewScanner(mf)
			for s.Scan() {
				// Use s.Bytes instead of s.Text to avoid a string allocation.
				b := s.Bytes()
				// Use IndexByte directly instead of strings.Split to avoid allocating a return slice.
				i := bytes.IndexByte(b, ':')
				if i < 0 {
					continue
				}
				k, v := b[:i], b[i+1:]
				if bytes.IndexByte(v, ':') >= 0 {
					continue
				}
				if string(k) == "Main-Class" {
					c.mainClass = strings.TrimSpace(string(v))
				} else if string(k) == "Implementation-Version" {
					c.version = strings.TrimSpace(string(v))
				}
			}
			if err := s.Err(); err != nil {
				return fmt.Errorf("scanning manifest file %s: %v", p, err)
			}
			return nil
		}

		// Scan for jars within jars.
		if !exts[path.Ext(p)] {
			return nil
		}
		// We've found a jar in a jar. Open it!
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to get archive inside of archive %s: %v", p, err)
		}
		// If we're about to read more than the max size we've configure ahead of time then stop.
		// Note that this only applies to embedded ZIPs/JARs. The outer ZIP/JAR can still be larger than the limit.
		if size+fi.Size() > maxZipSize {
			return fmt.Errorf("archive inside archive at %q is greater than 4GB, skipping", p)
		}
		f, err := r.Open(p)
		if err != nil {
			return fmt.Errorf("open file %s: %v", p, err)
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("read file %s: %v", p, err)
		}
		br := bytes.NewReader(data)
		r2, err := zip.NewReader(br, br.Size())
		if err != nil {
			if err == zip.ErrFormat {
				// Not a zip file.
				return nil
			}
			return fmt.Errorf("parsing file %s: %v", p, err)
		}
		if err := c.checkJAR(&zipFS{r2}, depth+1, size+fi.Size()); err != nil {
			return fmt.Errorf("checking sub jar %s: %v", p, err)
		}
		return nil
	})
	return err
}

var (
	// Replicate YARA rule:
	//
	// strings:
	// $JndiManagerConstructor = {
	//     3c 69 6e 69 74 3e ?? ?? ?? 28 4c 6a 61 76 61 2f 6c 61 6e 67 2f 53 74 72 69
	//     6e 67 3b 4c 6a 61 76 61 78 2f 6e 61 6d 69 6e 67 2f 43 6f 6e 74 65 78 74 3b
	//     29 56
	// }
	//
	// https://github.com/darkarnium/Log4j-CVE-Detect/blob/main/rules/vulnerability/log4j/CVE-2021-44228.yar
	log4JYARAPrefix = []byte{0x3c, 0x69, 0x6e, 0x69, 0x74, 0x3e}
	log4JYARASuffix = []byte{
		0x28, 0x4c, 0x6a, 0x61, 0x76, 0x61, 0x2f, 0x6c,
		0x61, 0x6e, 0x67, 0x2f, 0x53, 0x74, 0x72, 0x69,
		0x6e, 0x67, 0x3b, 0x4c, 0x6a, 0x61, 0x76, 0x61,
		0x78, 0x2f, 0x6e, 0x61, 0x6d, 0x69, 0x6e, 0x67,
		0x2f, 0x43, 0x6f, 0x6e, 0x74, 0x65, 0x78, 0x74,
		0x3b, 0x29, 0x56,
	}

	// Relevant commit: https://github.com/apache/logging-log4j2/commit/44569090f1cf1e92c711fb96dfd18cd7dccc72ea
	// In 2.16 the JndiManager class added the method `isJndiEnabled`. This was
	// done so the Interpolator could check if JNDI was enabled. We expect the
	// existence of this method should be relatively stable over time.
	//
	// This is definitely a bit brittle and may mean we fail to detect future versions
	// correctly (e.g. if there is a 2.17 that changes the name of the method).
	// What we really would like is something that was removed (a method, a
	// constructor, a string, anything...) in 2.16. But there isn't anything
	// so we have to rely on this brittle solution.
	//
	// Since this is so brittle, we're keeping the above rule that can reliably and
	// non-brittle-ey detect <2.15 as a back up.
	log4j216Detector = []byte("isJndiEnabled")
)

func matchesLog4JYARARule(b []byte) bool {
	start := 0
	for {
		i := bytes.Index(b[start:], log4JYARAPrefix)
		if i < 0 {
			return false
		}
		n := i + len(log4JYARAPrefix)
		if len(b) <= n {
			return false
		}
		j := bytes.Index(b[n:], log4JYARASuffix)
		if j < 0 {
			return false
		}
		if (j - i) <= 3 {
			return true
		}
		start = i + len(log4JYARAPrefix)
	}
}

func matchesTwoSixteen(b []byte) bool {
	return bytes.Contains(b, log4j216Detector)
}
