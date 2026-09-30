package signatures

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/wlame/rx-go/internal/config"
)

// LoadOptions says what Load reads and checks.
type LoadOptions struct {
	// Paths are the user files and directories to load, in order. Nil
	// means SearchPath(); an empty, non-nil slice loads the built-ins
	// only.
	Paths []string
	// CheckExamples, when set, runs a signature's examples: it is called
	// once for each loaded signature that has examples, and an error
	// refuses that signature. Nil means examples are not run. The scan
	// engine provides the checker; the loader cannot match lines itself.
	CheckExamples func(*Signature) error
}

// builtinDir is the directory of the built-in signature files inside
// the binary. groupsFile, in it, declares the built-in groups and their
// order; it loads first.
const (
	builtinDir = "builtin"
	groupsFile = "groups.yml"
)

// builtinFiles holds the built-in signature files.
//
// Go note: the //go:embed directive makes the compiler copy every file
// that matches the pattern into the binary; builtinFiles then reads
// them like a read-only file system. A built-in needs no file on the
// machine at run time, and no user can change it.
//
//go:embed builtin/*.yml
var builtinFiles embed.FS

// SearchPath returns where user signature files are looked for: the
// entries of RX_SIGNATURES_PATH (directories or files, separated by
// ":") when it is set and not empty; otherwise the first of
// $XDG_CONFIG_HOME/rx/signatures and ~/.config/rx/signatures that is a
// directory, or the first of them when neither is.
//
// Paths come only from the environment, never from a request.
func SearchPath() []string {
	if entries := config.GetPathSepEnv("RX_SIGNATURES_PATH"); len(entries) > 0 {
		return entries
	}
	candidates := configDirCandidates()
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return []string{dir}
		}
	}
	if len(candidates) == 0 {
		return []string{}
	}
	return candidates[:1]
}

// configDirCandidates are the signature directories under the user's
// configuration directories, in the order SearchPath tries them.
func configDirCandidates() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "rx", "signatures"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "rx", "signatures"))
	}
	return dirs
}

// Load reads the built-in signatures and the user files of
// opts.Paths, and assembles the catalogue. It never fails: every
// problem is collected in Catalogue.Errors, and what could be loaded is
// loaded.
//
// Order of work: every file is parsed first (built-ins, then user
// files in search-path order), so a group a later file declares can be
// used by an earlier one; then groups, signatures, overrides and
// disables are resolved in load order; then the examples check runs.
func Load(opts LoadOptions) *Catalogue {
	paths := opts.Paths
	if paths == nil {
		paths = SearchPath()
	}
	l := &loader{fileOrder: map[string]int{}}
	builtins := l.readBuiltins()
	files := l.listUserFiles(paths)
	users := l.readUserFiles(files)
	cat := l.assemble(builtins, users, opts.CheckExamples)
	cat.paths = slices.Clone(paths)
	cat.files = files
	return cat
}

// loader holds the state of one Load.
type loader struct {
	errors []LoadError
	// fileOrder numbers every file and path entry in load order, so the
	// errors can be sorted by file.
	fileOrder map[string]int
}

// fail records a problem.
func (l *loader) fail(e LoadError) {
	l.errors = append(l.errors, e)
}

// register numbers a file or path entry in load order.
func (l *loader) register(name string) {
	if _, ok := l.fileOrder[name]; !ok {
		l.fileOrder[name] = len(l.fileOrder)
	}
}

// readBuiltins parses the embedded files: groups.yml first, then the
// others in name order.
func (l *loader) readBuiltins() []*fileDef {
	entries, err := fs.ReadDir(builtinFiles, builtinDir)
	if err != nil {
		l.fail(LoadError{File: builtinDir, Message: "cannot list the built-in signatures: " + err.Error()})
		return nil
	}
	names := []string{groupsFile}
	for _, e := range entries {
		if e.Name() != groupsFile {
			names = append(names, e.Name())
		}
	}
	defs := make([]*fileDef, 0, len(names))
	for _, name := range names {
		embedded := path.Join(builtinDir, name)
		l.register(embedded)
		data, err := builtinFiles.ReadFile(embedded)
		if err != nil {
			l.fail(LoadError{File: embedded, Message: "cannot read: " + err.Error()})
			continue
		}
		if def := l.parse(embedded, data, true); def != nil {
			defs = append(defs, def)
		}
	}
	return defs
}

// parse parses one file, recording a whole-file refusal.
func (l *loader) parse(name string, data []byte, builtin bool) *fileDef {
	def, refusal := parseFile(name, data, builtin)
	if refusal != nil {
		l.fail(*refusal)
		return nil
	}
	return def
}

// signatureFileExts are the extensions of the files a directory on the
// search path contributes.
var signatureFileExts = []string{".yml", ".yaml"}

// listUserFiles lists the user files of the search path in load order:
// each entry in turn; a directory's *.yml and *.yaml files in name
// order, without recursion and without names that start with a dot
// (editor lock files such as .#team.yml); a file named on its own,
// whatever its name. A missing entry is skipped; a file reached twice
// is read once.
//
// SECURITY: these paths come from the environment, not from a request,
// and are configuration rather than searched files, so they are read
// directly rather than through the --search-root sandbox (which would
// refuse ~/.config under `rx serve --search-root=/var/log`).
func (l *loader) listUserFiles(paths []string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(file string) {
		key := file
		if abs, err := filepath.Abs(file); err == nil {
			key = abs
		}
		if !seen[key] {
			seen[key] = true
			l.register(file)
			files = append(files, file)
		}
	}
	for _, entry := range paths {
		l.register(entry)
		info, err := os.Stat(entry)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			l.fail(LoadError{File: entry, Message: "cannot read: " + err.Error()})
		case info.IsDir():
			for _, file := range l.directoryFiles(entry) {
				add(file)
			}
		default:
			add(entry)
		}
	}
	return files
}

// directoryFiles lists the signature files of one directory in name
// order.
func (l *loader) directoryFiles(dir string) []string {
	// Go note: os.ReadDir returns the entries sorted by name.
	entries, err := os.ReadDir(dir)
	if err != nil {
		l.fail(LoadError{File: dir, Message: "cannot read the directory: " + err.Error()})
		return nil
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !slices.Contains(signatureFileExts, filepath.Ext(name)) {
			continue
		}
		full := filepath.Join(dir, name)
		// os.Stat follows a symbolic link, so a link to a file counts
		// as that file and a link to a directory is skipped like one.
		info, err := os.Stat(full)
		if err == nil && info.IsDir() {
			continue
		}
		files = append(files, full)
	}
	return files
}

// readUserFiles parses the user files in order. A file whose
// signatures would bring the user signatures past maxUserSignatures is
// refused as a whole.
func (l *loader) readUserFiles(files []string) []*fileDef {
	defs := make([]*fileDef, 0, len(files))
	count := 0
	for _, file := range files {
		data, refusal := readUserFile(file)
		if refusal != nil {
			l.fail(*refusal)
			continue
		}
		def := l.parse(file, data, false)
		if def == nil {
			continue
		}
		if count+len(def.entries) > maxUserSignatures {
			first := def.entries[maxUserSignatures-count]
			l.fail(LoadError{File: file, Line: first.line, Message: fmt.Sprintf(
				"more than %d user signatures: this file brings the count to %d%s",
				maxUserSignatures, count+len(def.entries), wholeFileSuffix)})
			continue
		}
		count += len(def.entries)
		defs = append(defs, def)
	}
	return defs
}

// readUserFile reads one user file, at most maxFileBytes of it.
//
// SECURITY: O_NONBLOCK makes the open return at once if the name leads
// to a named pipe, which would otherwise wait for a writer for ever;
// the check after the open refuses anything that is not a regular
// file. The read stops one byte past the limit, so a file that grows
// while it is read costs no more than that.
func readUserFile(file string) ([]byte, *LoadError) {
	refuse := func(msg string) ([]byte, *LoadError) {
		return nil, &LoadError{File: file, Message: msg + wholeFileSuffix}
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return refuse("cannot read: " + err.Error())
	}
	// Go note: defer runs f.Close when the function returns, on every
	// path below.
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return refuse("cannot read: " + err.Error())
	}
	if !info.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	tooLarge := func(size int64) ([]byte, *LoadError) {
		return refuse(fmt.Sprintf("the file is %d bytes, over the limit of %d bytes", size, maxFileBytes))
	}
	if info.Size() > maxFileBytes {
		return tooLarge(info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return refuse("cannot read: " + err.Error())
	}
	if len(data) > maxFileBytes {
		return tooLarge(int64(len(data)))
	}
	return data, nil
}

// assembly is the state of the catalogue while the parsed files are
// put together.
type assembly struct {
	*loader
	groups     []Group
	groupOrder map[string]int
	// active is the definition each id runs with.
	active map[string]*Signature
	// builtins are the built-in definitions by id; userDefined the user
	// definitions that loaded.
	builtins, userDefined map[string]*Signature
	disabled              []*Signature
	refused               map[string][]LoadError
	seq                   int
}

// assemble puts the parsed files together into a catalogue.
func (l *loader) assemble(builtins, users []*fileDef, checkExamples func(*Signature) error) *Catalogue {
	a := &assembly{
		loader: l, groupOrder: map[string]int{}, active: map[string]*Signature{},
		builtins: map[string]*Signature{}, userDefined: map[string]*Signature{}, refused: map[string][]LoadError{},
	}
	for _, def := range builtins {
		a.addGroups(def)
	}
	builtinGroupCount := len(a.groups)
	for _, def := range users {
		a.addGroups(def)
	}
	for _, def := range builtins {
		a.addBuiltinSignatures(def, builtinGroupCount)
	}
	for _, def := range users {
		a.addUserSignatures(def)
	}
	for _, def := range users {
		a.applyDisables(def)
	}
	if checkExamples != nil {
		a.checkExamples(checkExamples)
	}
	return a.catalogue(checkExamples != nil)
}

// addGroups declares a file's groups after those already known.
func (a *assembly) addGroups(def *fileDef) {
	for _, e := range def.errs {
		a.fail(e)
	}
	for _, g := range def.groups {
		if order, taken := a.groupOrder[g.name]; taken {
			a.fail(LoadError{File: def.name, Line: g.line, Message: fmt.Sprintf(
				"group %s is declared already (%s)", g.name, groupPlace(a.groups[order-1]))})
			continue
		}
		group := Group{Name: g.name, Description: g.description, Order: len(a.groups) + 1, Builtin: def.builtin}
		a.groups = append(a.groups, group)
		a.groupOrder[g.name] = group.Order
	}
}

// groupPlace says where a group was declared, for an error message.
func groupPlace(g Group) string {
	if g.Builtin {
		return "a built-in group"
	}
	return "by a user file"
}

// refuse records the errors of a definition that did not load, under
// its id.
func (a *assembly) refuse(id string, errs ...LoadError) {
	for _, e := range errs {
		a.fail(e)
	}
	if id != "" {
		a.refused[id] = append(a.refused[id], errs...)
	}
}

// accept makes sig the definition of its id.
func (a *assembly) accept(sig *Signature) {
	a.seq++
	sig.seq = a.seq
	a.active[sig.ID] = sig
}

// addBuiltinSignatures adds a built-in file's signatures; they may use
// the built-in groups only.
func (a *assembly) addBuiltinSignatures(def *fileDef, builtinGroups int) {
	for _, e := range def.entries {
		if e.sig == nil {
			a.refuse(e.id, e.errs...)
			continue
		}
		if order := a.groupOrder[e.sig.Group]; order == 0 || order > builtinGroups {
			a.refuse(e.id, unknownGroup(def.name, e))
			continue
		}
		if prev, dup := a.builtins[e.id]; dup {
			a.refuse(e.id, duplicateID(def.name, e, prev))
			continue
		}
		a.builtins[e.id] = e.sig
		a.accept(e.sig)
	}
}

// addUserSignatures adds a user file's signatures. One with a
// built-in's id replaces it (Overrides); a refused one with a
// built-in's id removes the built-in too, since the user meant to
// replace it; the same id twice among user files is an error of the
// later definition.
func (a *assembly) addUserSignatures(def *fileDef) {
	for _, e := range def.entries {
		var problem *LoadError
		switch {
		case e.sig == nil:
		case a.groupOrder[e.sig.Group] == 0:
			err := unknownGroup(def.name, e)
			problem = &err
		case a.userDefined[e.id] != nil:
			err := duplicateID(def.name, e, a.userDefined[e.id])
			problem = &err
		}
		if e.sig == nil || problem != nil {
			errs := e.errs
			if problem != nil {
				errs = []LoadError{*problem}
			}
			a.refuse(e.id, errs...)
			if a.builtins[e.id] != nil && a.userDefined[e.id] == nil {
				delete(a.active, e.id)
			}
			continue
		}
		e.sig.Overrides = a.builtins[e.id] != nil
		a.userDefined[e.id] = e.sig
		a.accept(e.sig)
	}
}

// unknownGroup is the error of a signature whose group no file
// declares.
func unknownGroup(file string, e entry) LoadError {
	return LoadError{File: file, Line: e.groupLine, ID: e.id, Message: fmt.Sprintf(
		"unknown group %q: use a built-in group or declare it under groups:", e.sig.Group)}
}

// duplicateID is the error of a later definition of an id.
func duplicateID(file string, e entry, first *Signature) LoadError {
	return LoadError{File: file, Line: e.line, ID: e.id, Message: fmt.Sprintf(
		"duplicate id: %s is defined already at %s:%d", e.id, first.Source.File, first.Source.Line)}
}

// applyDisables switches off the built-ins a user file's `disable:`
// names. An id that is not a built-in's is an error of the file; a
// built-in a user signature replaced has nothing left to switch off.
func (a *assembly) applyDisables(def *fileDef) {
	for _, ref := range def.disable {
		builtin := a.builtins[ref.id]
		switch {
		case builtin == nil && a.userDefined[ref.id] != nil:
			a.fail(LoadError{File: def.name, Line: ref.line, Message: fmt.Sprintf(
				"disable: %s is not a built-in signature; disable switches built-ins off", ref.id)})
		case builtin == nil:
			a.fail(LoadError{File: def.name, Line: ref.line, Message: fmt.Sprintf(
				"disable: no built-in signature has the id %q", ref.id)})
		case a.active[ref.id] == builtin:
			builtin.DisabledBy = &Source{File: def.name, Line: ref.line}
			delete(a.active, ref.id)
			a.disabled = append(a.disabled, builtin)
		}
	}
}

// checkExamples runs the examples check on every active signature that
// has examples, in load order; a failure refuses the signature.
func (a *assembly) checkExamples(check func(*Signature) error) {
	for _, sig := range a.sorted(mapValues(a.active)) {
		if len(sig.Examples.Match)+len(sig.Examples.NoMatch) == 0 {
			continue
		}
		if err := check(sig); err != nil {
			a.refuse(sig.ID, LoadError{File: sig.Source.File, Line: sig.examplesLine, ID: sig.ID, Message: "examples: " + err.Error()})
			delete(a.active, sig.ID)
		}
	}
}

// sorted orders signatures by group order, then load order.
func (a *assembly) sorted(sigs []*Signature) []*Signature {
	slices.SortFunc(sigs, func(x, y *Signature) int {
		if c := cmp.Compare(a.groupOrder[x.Group], a.groupOrder[y.Group]); c != 0 {
			return c
		}
		return cmp.Compare(x.seq, y.seq)
	})
	return sigs
}

// mapValues returns the values of a map in no particular order.
func mapValues(m map[string]*Signature) []*Signature {
	out := make([]*Signature, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// catalogue builds the finished catalogue.
func (a *assembly) catalogue(examplesChecked bool) *Catalogue {
	cat := &Catalogue{
		signatures:      a.sorted(mapValues(a.active)),
		disabled:        a.sorted(slices.Clone(a.disabled)),
		groups:          a.groups,
		byID:            map[string]*Signature{},
		disabledByID:    map[string]*Signature{},
		refused:         map[string][]LoadError{},
		examplesChecked: examplesChecked,
	}
	for _, s := range cat.signatures {
		cat.byID[s.ID] = s
	}
	for _, s := range cat.disabled {
		cat.disabledByID[s.ID] = s
	}
	for id, errs := range a.refused {
		if cat.byID[id] == nil && cat.disabledByID[id] == nil {
			cat.refused[id] = errs
		}
	}
	slices.SortStableFunc(a.errors, func(x, y LoadError) int {
		if c := cmp.Compare(a.fileOrder[x.File], a.fileOrder[y.File]); c != 0 {
			return c
		}
		return cmp.Compare(x.Line, y.Line)
	})
	cat.errors = a.errors
	return cat
}
