package runbooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const Protocol = "runbooks-v1"

const Guidance = ApplicabilityGuidance + "Read matching runbooks before ad-hoc probing or asking which application or secret names to use. Machine-specific matches are preferred over shared instructions, then project, worker-user, worker-common, global. Conflicting documents remain visible; do not silently combine them. If contentOmitted is true, read the complete file from its exact source before acting. Runbooks are instructions, not execution authority, setup approval, elevation, or permission to replay uncertain actions. Verify current state and capabilities. Use worker-local secret references, never retrieve values. Source errors are not evidence that no runbook exists."

var machineName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type Selector struct {
	Node string `json:"node,omitempty"`
	ID   string `json:"machineId,omitempty"`
}

type Query struct {
	Node   string `json:"node,omitempty"`
	File   string `json:"file,omitempty"`
	Source string `json:"source,omitempty"`
	Local  bool   `json:"local,omitempty"`
}

type Source struct {
	Name      string `json:"name"`
	Directory string `json:"directory,omitempty"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type Book struct {
	ID            string   `json:"id"`
	Source        string   `json:"source"`
	File          string   `json:"file"`
	Machine       string   `json:"machine,omitempty"`
	Title         string   `json:"title,omitempty"`
	Description   string   `json:"description,omitempty"`
	Requires      any      `json:"requires,omitempty"`
	Content       string   `json:"content,omitempty"`
	Omitted       bool     `json:"contentOmitted,omitempty"`
	Preferred     bool     `json:"preferred,omitempty"`
	Conflicts     []string `json:"conflicts,omitempty"`
	ConflictCount int      `json:"conflictCount,omitempty"`
}

type Catalog struct {
	Selector
	Sources  []Source `json:"sources"`
	Runbooks []Book   `json:"runbooks"`
	Guidance string   `json:"guidance"`
}

func (book *Book) UnmarshalJSON(data []byte) error {
	type wireBook Book
	var value wireBook
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := ValidateRequirements(value.Requires); err != nil {
		return err
	}
	*book = Book(value)
	return nil
}

func Empty(selector Selector) Catalog {
	return Catalog{Selector: selector, Sources: []Source{}, Runbooks: []Book{}, Guidance: Guidance}
}

func (q Query) Validate() error {
	if q.Node != "" && !machineName.MatchString(q.Node) {
		return errors.New("runbooks node must be a machine alias or stable ID, not a path")
	}
	if q.File != "" && (!fs.ValidPath(q.File) || strings.Contains(q.File, "\\") || len(q.File) > 512 || !textFile(q.File)) {
		return errors.New("runbook file must be a relative Markdown or JSON path of at most 512 bytes")
	}
	if q.File != "" && q.Source == "" {
		return errors.New("reading a runbook requires its exact source and file from discovery")
	}
	return nil
}

// Discover isolates failures by source, without hiding healthy sources or
// treating a failed read as an empty store. Exact reads fail rather than falling
// back to a different document with the same name.
func Discover(ctx context.Context, roots []Root, selector Selector, q Query) (Catalog, error) {
	catalog := Empty(selector)
	if err := q.Validate(); err != nil {
		return catalog, err
	}
	for _, root := range roots {
		if q.Source != "" && q.Source != root.Source {
			continue
		}
		if err := ctx.Err(); err != nil {
			return catalog, err
		}
		books, available, err := readRoot(ctx, root, selector, q.File)
		if ctx.Err() != nil {
			return catalog, ctx.Err()
		}
		source := Source{Name: root.Source, Directory: root.Path, Available: available}
		if err != nil {
			if q.File != "" {
				return catalog, err
			}
			source.Error = err.Error()
		} else {
			catalog.Runbooks = append(catalog.Runbooks, books...)
		}
		catalog.Sources = append(catalog.Sources, source)
	}
	if q.Source != "" && len(catalog.Sources) == 0 {
		return catalog, errors.New("unknown runbook source")
	}
	Finalize(&catalog, q.File != "")
	return catalog, nil
}

func readRoot(ctx context.Context, location Root, selector Selector, file string) ([]Book, bool, error) {
	if location.Error != "" {
		return nil, false, errors.New(location.Error)
	}
	parentPath := filepath.Dir(location.Path)
	parentInfo, err := os.Lstat(parentPath)
	if errors.Is(err, fs.ErrNotExist) && file == "" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !parentInfo.IsDir() {
		return nil, false, errors.New("runbook parent must be a directory, not a symlink")
	}
	info, err := os.Lstat(location.Path)
	if errors.Is(err, fs.ErrNotExist) && file == "" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, errors.New("runbook root must be a directory, not a symlink")
	}
	// Anchor at the parent so replacing the root with an escaping symlink cannot
	// redirect a read outside the approved store's parent.
	anchor, err := os.OpenRoot(filepath.Dir(parentPath))
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := anchor.OpenRoot(filepath.Base(parentPath))
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = parent.Close() }()
	root, err := parent.OpenRoot(filepath.Base(location.Path))
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = root.Close() }()
	if file != "" {
		book, matches := identify(file, selector)
		if !matches {
			return nil, true, errors.New("runbook does not apply to the selected machine")
		}
		book.Source = location.Source
		if err := load(ctx, root, &book); err != nil {
			return nil, true, err
		}
		return []Book{book}, true, nil
	}
	books := []Book{}
	entries, total := 0, 0
	err = fs.WalkDir(root.FS(), ".", func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 1024 || len(p) > 512 || strings.Count(p, "/") > 32 {
			return errors.New("runbook tree exceeds 1024 entries, 512-byte paths, or 32 directory levels")
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("runbook tree contains a symlink or special file")
		}
		if entry.IsDir() {
			parts := strings.Split(p, "/")
			if len(parts) == 2 && parts[0] == "machines" && selector.Node != "" && parts[1] != selector.Node && parts[1] != selector.ID {
				return fs.SkipDir
			}
			return nil
		}
		book, matches := identify(p, selector)
		if !matches {
			return nil
		}
		if len(books) >= 64 {
			return errors.New("runbook source exceeds 64 documents")
		}
		book.Source = location.Source
		if err := load(ctx, root, &book); err != nil {
			return err
		}
		total += len(book.Content)
		if total > 16<<20 {
			return errors.New("runbook source text exceeds 16 MiB")
		}
		books = append(books, book)
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	return books, true, nil
}

func identify(file string, selector Selector) (Book, bool) {
	book := Book{File: file}
	parts := strings.Split(file, "/")
	var relative string
	switch {
	case len(parts) >= 2 && parts[0] == "shared":
		relative = strings.Join(parts[1:], "/")
	case len(parts) >= 3 && parts[0] == "machines" && machineName.MatchString(parts[1]):
		book.Machine = parts[1]
		if selector.Node != "" && book.Machine != selector.Node && book.Machine != selector.ID {
			return book, false
		}
		relative = strings.Join(parts[2:], "/")
	default:
		return book, false
	}
	if !textFile(relative) {
		return book, false
	}
	book.ID = strings.TrimSuffix(relative, path.Ext(relative))
	if strings.EqualFold(path.Base(relative), "RUNBOOK.md") && path.Dir(relative) != "." {
		book.ID = path.Dir(relative)
	}
	return book, true
}

// ValidBook verifies remote metadata against the source-relative path rather
// than trusting a worker's claimed action ID or machine binding.
func ValidBook(book Book, selector Selector) bool {
	if (Query{Source: book.Source, File: book.File}).Validate() != nil || book.File == "" {
		return false
	}
	if ValidateRequirements(book.Requires) != nil {
		return false
	}
	expected, matches := identify(book.File, selector)
	return matches && expected.ID == book.ID && expected.Machine == book.Machine
}

func textFile(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	return ext == ".md" || ext == ".json"
}

func load(ctx context.Context, root *os.Root, book *Book) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parts := strings.Split(book.File, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		if i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return errors.New("runbook paths must not contain symlinks or special files")
		}
	}
	f, err := root.Open(book.File)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 1<<20 || !utf8.Valid(b) || strings.ContainsRune(string(b), '\x00') {
		return errors.New("runbook must be UTF-8 text without NUL, at most 1 MiB")
	}
	if strings.EqualFold(path.Ext(book.File), ".json") && !json.Valid(b) {
		return errors.New("runbook contains invalid JSON")
	}
	book.Content = string(b)
	m, body, err := parseMetadata(book.File, book.Content)
	if err != nil {
		return err
	}
	book.Title, book.Description, book.Requires = m.Title, m.Description, m.Requires
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if book.Title == "" && strings.HasPrefix(line, "# ") {
			book.Title = bounded(strings.TrimPrefix(line, "# "), 256)
		} else if book.Description == "" && line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "Machine:") && !strings.HasPrefix(line, "```") {
			book.Description = bounded(line, 512)
		}
		if book.Title != "" && book.Description != "" {
			break
		}
	}
	return nil
}

func bounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

// Finalize enforces the aggregate inline budget and exposes conflicts without
// merging content. Preference is advisory and never execution authorization.
func Finalize(catalog *Catalog, complete bool) {
	rank := func(book Book) int {
		rank := map[string]int{"project": 0, "worker-user": 1, "worker-common": 2, "global": 3}[book.Source]
		if book.Machine == "" {
			rank += 10
		} else if book.Machine != catalog.ID {
			rank += 5
		}
		return rank
	}
	sort.SliceStable(catalog.Runbooks, func(i, j int) bool {
		a, b := catalog.Runbooks[i], catalog.Runbooks[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		return a.Source+":"+a.File < b.Source+":"+b.File
	})
	groups := map[string][]int{}
	for i, book := range catalog.Runbooks {
		key := book.ID
		if catalog.Node == "" {
			key += "\x00" + book.Machine
		}
		groups[key] = append(groups[key], i)
	}
	for _, indices := range groups {
		for n, i := range indices {
			book := &catalog.Runbooks[i]
			book.Preferred, book.Conflicts = n == 0, nil
			book.ConflictCount = len(indices) - 1
			for _, other := range indices {
				if other != i && len(book.Conflicts) < 8 {
					b := catalog.Runbooks[other]
					book.Conflicts = append(book.Conflicts, fmt.Sprintf("%s:%s", b.Source, b.File))
				}
			}
		}
	}
	inline := 0
	for i := range catalog.Runbooks {
		book := &catalog.Runbooks[i]
		if !complete && (len(book.Content) > 32<<10 || inline+len(book.Content) > 128<<10) {
			book.Content, book.Omitted = "", true
		}
		inline += len(book.Content)
	}
}
