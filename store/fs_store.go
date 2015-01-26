package store

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/kr/fs"

	"sort"

	"sourcegraph.com/sourcegraph/rwvfs"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

// useIndexedStore indicates whether the indexed{Unit,Tree}Stores
// should be used. If it's false, only the FS-backed stores are used
// (which requires full scans for all filters).
var (
	noIndex, _      = strconv.ParseBool(os.Getenv("NOINDEX"))
	useIndexedStore = !noIndex
)

// A fsMultiRepoStore is a MultiRepoStore that stores data on a VFS.
type fsMultiRepoStore struct {
	fs rwvfs.WalkableFileSystem
	FSMultiRepoStoreConf
	repoStores
}

var _ MultiRepoStoreImporter = (*fsMultiRepoStore)(nil)

// NewFSMultiRepoStore creates a new repository store (that can be
// imported into) that is backed by files on a filesystem.
func NewFSMultiRepoStore(fs rwvfs.WalkableFileSystem, conf *FSMultiRepoStoreConf) MultiRepoStoreImporter {
	if conf == nil {
		conf = &FSMultiRepoStoreConf{}
	}
	if conf.RepoPaths == nil {
		conf.RepoPaths = DefaultRepoPaths
	}

	setCreateParentDirs(fs)
	mrs := &fsMultiRepoStore{fs: fs, FSMultiRepoStoreConf: *conf}
	mrs.repoStores = repoStores{mrs}
	return mrs
}

// FSMultiRepoStoreConf configures an FS-backed multi-repo store. Pass
// it to NewFSMultiRepoStore to construct a new store with the
// specified options.
type FSMultiRepoStoreConf struct {
	// RepoPathConfig specifies where the multi-repo store stores
	// repository data. If nil, DefaultRepoPaths is used, which stores
	// repos at "${REPO}/.srclib-store".
	RepoPaths
}

func (s *fsMultiRepoStore) Repos(f ...RepoFilter) ([]string, error) {
	scopeRepos, err := scopeRepos(storeFilters(f))
	if err != nil {
		return nil, err
	}

	// Multiple repos are mutually exclusive.
	if len(scopeRepos) > 1 {
		return nil, nil
	}

	var after string
	var max int
	if len(scopeRepos) == 1 {
		afterComps := s.RepoToPath(scopeRepos[0])
		if len(afterComps) > 0 {
			lastComp := afterComps[len(afterComps)-1]
			// We want to include the scoped repo, so make "after"
			// into a string that sorts lexicographically BEFORE the
			// scoped repo.
			lastComp = lastComp[:len(lastComp)-1] + string([]rune{rune(lastComp[len(lastComp)-1]) - rune(1)}) + "\xff"
			afterComps[len(afterComps)-1] = lastComp
		}
		after = s.fs.Join(afterComps...)
		max = 1
	}

	paths, err := s.ListRepoPaths(s.fs, after, max)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, path := range paths {
		repo := s.PathToRepo(path)
		if repoFilters(f).SelectRepo(repo) {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

func (s *fsMultiRepoStore) openRepoStore(repo string) RepoStore {
	subpath := s.fs.Join(s.RepoToPath(repo)...)
	return NewFSRepoStore(rwvfs.Sub(s.fs, subpath))
}

func (s *fsMultiRepoStore) openAllRepoStores() (map[string]RepoStore, error) {
	repos, err := s.Repos()
	if err != nil {
		return nil, err
	}

	rss := make(map[string]RepoStore, len(repos))
	for _, repo := range repos {
		rss[repo] = s.openRepoStore(repo)
	}
	return rss, nil
}

var _ repoStoreOpener = (*fsMultiRepoStore)(nil)

func (s *fsMultiRepoStore) Import(repo, commitID string, unit *unit.SourceUnit, data graph.Output) error {
	if unit != nil {
		cleanForImport(&data, repo, unit.Type, unit.Name)
	}
	subpath := s.fs.Join(s.RepoToPath(repo)...)
	if err := rwvfs.MkdirAll(s.fs, subpath); err != nil {
		return err
	}
	return s.openRepoStore(repo).(RepoImporter).Import(commitID, unit, data)
}

func (s *fsMultiRepoStore) String() string { return "fsMultiRepoStore" }

// A fsRepoStore is a RepoStore that stores data on a VFS.
type fsRepoStore struct {
	fs rwvfs.FileSystem
	treeStores
}

// SrclibStoreDir is the name of the directory under which a RepoStore's data is stored.
const SrclibStoreDir = ".srclib-store"

// NewFSRepoStore creates a new repository store (that can be
// imported into) that is backed by files on a filesystem.
func NewFSRepoStore(fs rwvfs.FileSystem) RepoStoreImporter {
	setCreateParentDirs(fs)
	rs := &fsRepoStore{fs: fs}
	rs.treeStores = treeStores{rs}
	return rs
}

func (s *fsRepoStore) Versions(f ...VersionFilter) ([]*Version, error) {
	versionDirs, err := s.versionDirs()
	if err != nil {
		return nil, err
	}

	var versions []*Version
	for _, dir := range versionDirs {
		version := &Version{CommitID: path.Base(dir)}
		if versionFilters(f).SelectVersion(version) {
			versions = append(versions, version)
		}
	}
	return versions, nil
}

func (s *fsRepoStore) versionDirs() ([]string, error) {
	entries, err := s.fs.ReadDir(".")
	if err != nil {
		return nil, err
	}
	dirs := make([]string, len(entries))
	for i, e := range entries {
		dirs[i] = e.Name()
	}
	return dirs, nil
}

func (s *fsRepoStore) Import(commitID string, unit *unit.SourceUnit, data graph.Output) error {
	if unit != nil {
		cleanForImport(&data, "", unit.Type, unit.Name)
	}
	ts := s.newTreeStore(commitID)
	return ts.Import(unit, data)
}

func (s *fsRepoStore) treeStoreFS(commitID string) rwvfs.FileSystem {
	return rwvfs.Sub(s.fs, commitID)
}

func (s *fsRepoStore) newTreeStore(commitID string) TreeStoreImporter {
	fs := s.treeStoreFS(commitID)
	if useIndexedStore {
		return newIndexedTreeStore(fs)
	}
	return newFSTreeStore(fs)
}

func (s *fsRepoStore) openTreeStore(commitID string) TreeStore {
	return s.newTreeStore(commitID)
}

func (s *fsRepoStore) openAllTreeStores() (map[string]TreeStore, error) {
	versionDirs, err := s.versionDirs()
	if err != nil {
		return nil, err
	}

	tss := make(map[string]TreeStore, len(versionDirs))
	for _, dir := range versionDirs {
		commitID := path.Base(dir)
		tss[commitID] = s.openTreeStore(commitID)
	}
	return tss, nil
}

var _ treeStoreOpener = (*fsRepoStore)(nil)

func (s *fsRepoStore) String() string { return "fsRepoStore" }

// A fsTreeStore is a TreeStore that stores data on a VFS.
type fsTreeStore struct {
	fs rwvfs.FileSystem
	unitStores
}

func newFSTreeStore(fs rwvfs.FileSystem) *fsTreeStore {
	ts := &fsTreeStore{fs: fs}
	ts.unitStores = unitStores{ts}
	return ts
}

func (s *fsTreeStore) Units(f ...UnitFilter) ([]*unit.SourceUnit, error) {
	unitFilenames, err := s.unitFilenames()
	if err != nil {
		return nil, err
	}

	var units []*unit.SourceUnit
	for _, filename := range unitFilenames {
		unit, err := s.openUnitFile(filename)
		if err != nil {
			return nil, err
		}
		if unitFilters(f).SelectUnit(unit) {
			units = append(units, unit)
		}
	}
	return units, nil
}

func (s *fsTreeStore) openUnitFile(filename string) (u *unit.SourceUnit, err error) {
	f, err := s.fs.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errUnitNoInit
		}
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	var unit unit.SourceUnit
	return &unit, Codec.Decode(f, &unit)
}

func (s *fsTreeStore) unitFilenames() ([]string, error) {
	var files []string
	w := fs.WalkFS(".", rwvfs.Walkable(s.fs))
	for w.Step() {
		if err := w.Err(); err != nil {
			return nil, err
		}
		fi := w.Stat()
		if fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), unitFileSuffix) {
			files = append(files, w.Path())
		}
	}
	return files, nil
}

func (s *fsTreeStore) unitFilename(unitType, unit string) string {
	return path.Join(unit, unitType+unitFileSuffix)
}

const unitFileSuffix = ".unit.json"

func (s *fsTreeStore) Import(u *unit.SourceUnit, data graph.Output) (err error) {
	if u == nil {
		return rwvfs.MkdirAll(s.fs, ".")
	}

	unitFilename := s.unitFilename(u.Type, u.Name)
	if err := rwvfs.MkdirAll(s.fs, path.Dir(unitFilename)); err != nil {
		return err
	}
	f, err := s.fs.Create(unitFilename)
	if err != nil {
		return err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()
	if err := Codec.Encode(f, u); err != nil {
		return err
	}

	dir := strings.TrimSuffix(unitFilename, unitFileSuffix)
	if err := rwvfs.MkdirAll(s.fs, dir); err != nil {
		return err
	}
	cleanForImport(&data, "", u.Type, u.Name)
	return s.openUnitStore(unit.ID2{Type: u.Type, Name: u.Name}).(UnitStoreImporter).Import(data)
}

func (s *fsTreeStore) openUnitStore(u unit.ID2) UnitStore {
	filename := s.unitFilename(u.Type, u.Name)
	dir := strings.TrimSuffix(filename, unitFileSuffix)
	if useIndexedStore {
		return newIndexedUnitStore(rwvfs.Sub(s.fs, dir))
	}
	return &fsUnitStore{fs: rwvfs.Sub(s.fs, dir)}
}

func (s *fsTreeStore) openAllUnitStores() (map[unit.ID2]UnitStore, error) {
	unitFiles, err := s.unitFilenames()
	if err != nil {
		return nil, err
	}

	uss := make(map[unit.ID2]UnitStore, len(unitFiles))
	for _, unitFile := range unitFiles {
		// TODO(sqs): duplicated code both here and in openUnitStore
		// for "dir" and "u".
		dir := strings.TrimSuffix(unitFile, unitFileSuffix)
		u := unit.ID2{Type: path.Base(dir), Name: path.Dir(dir)}
		uss[u] = s.openUnitStore(u)
	}
	return uss, nil
}

var _ unitStoreOpener = (*fsTreeStore)(nil)

func (s *fsTreeStore) String() string { return "fsTreeStore" }

// A fsUnitStore is a UnitStore that stores data on a VFS.
//
// It is typically wrapped by an indexedUnitStore, which provides fast
// responses to indexed queries and passes non-indexed queries through
// to this underlying fsUnitStore.
type fsUnitStore struct {
	// fs is the filesystem where data (and indexes, if
	// fsUnitStore is wrapped by an indexedUnitStore) are
	// written to and read from. The store may create multiple files
	// and arbitrary directory trees in fs (for indexes, etc.).
	fs rwvfs.FileSystem
}

const (
	unitDefsFilename = "def.dat"
	unitRefsFilename = "ref.dat"
)

func (s *fsUnitStore) Defs(fs ...DefFilter) (defs []*graph.Def, err error) {
	f, err := s.fs.Open(unitDefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	for {
		var def *graph.Def
		if err := Codec.Decode(f, &def); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if defFilters(fs).SelectDef(def) {
			defs = append(defs, def)
		}
	}
	return defs, nil
}

// defsAtOffsets reads the defs at the given serialized byte offsets
// from the def data file and returns them in arbitrary order.
func (s *fsUnitStore) defsAtOffsets(ofs byteOffsets, fs []DefFilter) (defs []*graph.Def, err error) {
	f, err := s.fs.Open(unitDefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	ffs := defFilters(fs)

	for _, ofs := range ofs {
		if _, err := f.Seek(ofs, 0); err != nil {
			return nil, err
		}
		var def *graph.Def
		if err := Codec.Decode(f, &def); err != nil {
			return nil, err
		}
		if ffs.SelectDef(def) {
			defs = append(defs, def)
		}
	}
	return defs, nil
}

// readDefs reads all defs from the def data file and returns them
// along with their serialized byte offsets.
func (s *fsUnitStore) readDefs() (defs []*graph.Def, ofs byteOffsets, err error) {
	f, err := s.fs.Open(unitDefsFilename)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	cr := &countingReader{Reader: f}
	for {
		o := cr.n

		var def *graph.Def
		if err := Codec.Decode(cr, &def); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, err
		}

		ofs = append(ofs, o)
		defs = append(defs, def)
	}
	return defs, ofs, nil
}

func (s *fsUnitStore) Refs(fs ...RefFilter) (refs []*graph.Ref, err error) {
	f, err := s.fs.Open(unitRefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	for {
		var ref *graph.Ref
		if err := Codec.Decode(f, &ref); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if refFilters(fs).SelectRef(ref) {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// refsAtByteRanges reads the refs at the given serialized byte ranges
// from the ref data file and returns them in arbitrary order.
func (s *fsUnitStore) refsAtByteRanges(brs []byteRanges, fs []RefFilter) (refs []*graph.Ref, err error) {
	f, err := s.fs.Open(unitRefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	// See how many bytes we need to read to get the refs in all
	// byteRanges.
	readLengths := make([]int64, len(brs))
	totalRefs := 0
	for i, br := range brs {
		var n int64
		for _, b := range br[1:] {
			n += b
			totalRefs++
		}
		readLengths[i] = n
	}

	ffs := refFilters(fs)

	for i, br := range brs {
		if _, err := f.Seek(br.start(), 0); err != nil {
			return nil, err
		}
		b, err := ioutil.ReadAll(io.LimitReader(f, readLengths[i]))
		if err != nil {
			return nil, err
		}

		r := bytes.NewReader(b)
		for range br[1:] {
			var ref *graph.Ref
			if err := Codec.Decode(r, &ref); err != nil {
				return nil, err
			}
			if ffs.SelectRef(ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs, nil
}

// readDefs reads all defs from the def data file and returns them
// along with their serialized byte offsets.
func (s *fsUnitStore) readRefs() (refs []*graph.Ref, fbrs fileByteRanges, err error) {
	f, err := s.fs.Open(unitRefsFilename)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	cr := &countingReader{Reader: f}
	fbrs = fileByteRanges{}
	lastFile := ""
	for {
		o := cr.n

		var ref *graph.Ref
		if err := Codec.Decode(cr, &ref); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, err
		}

		var lastFileRefStartOffset int64
		if brs, present := fbrs[ref.File]; present {
			lastFileRefStartOffset = brs[len(brs)-1]
		}
		if lastFile != "" && ref.File != lastFile {
			fbrs[lastFile] = append(fbrs[lastFile], o-lastFileRefStartOffset)
		}
		fbrs[ref.File] = append(fbrs[ref.File], o-lastFileRefStartOffset)
		refs = append(refs, ref)
		lastFile = ref.File
	}
	if lastFile != "" {
		var lastFileRefStartOffset int64
		if brs, present := fbrs[lastFile]; present {
			lastFileRefStartOffset = brs[len(brs)-1]
		}
		fbrs[lastFile] = append(fbrs[lastFile], cr.n-lastFileRefStartOffset)
	}
	return refs, fbrs, nil

}

func (s *fsUnitStore) Import(data graph.Output) error {
	cleanForImport(&data, "", "", "")
	if _, err := s.writeDefs(data.Defs); err != nil {
		return err
	}
	if _, err := s.writeRefs(data.Refs); err != nil {
		return err
	}
	return nil
}

// writeDefs writes the def data file. It also tracks (in ofs) the
// serialized byte offset where each def's serialized representation
// begins (which is used during index construction).
func (s *fsUnitStore) writeDefs(defs []*graph.Def) (ofs byteOffsets, err error) {
	f, err := s.fs.Create(unitDefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	cw := &countingWriter{Writer: f}
	ofs = make(byteOffsets, len(defs))
	for i, def := range defs {
		ofs[i] = cw.n
		if err := Codec.Encode(cw, def); err != nil {
			return nil, err
		}
	}
	return ofs, nil
}

// writeDefs writes the ref data file.
func (s *fsUnitStore) writeRefs(refs []*graph.Ref) (fbr fileByteRanges, err error) {
	f, err := s.fs.Create(unitRefsFilename)
	if err != nil {
		return nil, err
	}
	defer func() {
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}()

	// Sort refs by file and start byte so that we can use streaming
	// reads to efficiently read in all of the refs that exist in a
	// file.
	sort.Sort(refsByFileStartEnd(refs))

	cw := &countingWriter{Writer: f}
	fbr = fileByteRanges{}
	lastFile := ""
	lastFileByteRanges := byteRanges{}
	for _, ref := range refs {
		if lastFile != ref.File {
			if lastFile != "" {
				fbr[lastFile] = lastFileByteRanges
			}
			lastFile = ref.File
			lastFileByteRanges = byteRanges{cw.n}
		}
		before := cw.n
		if err := Codec.Encode(cw, ref); err != nil {
			return nil, err
		}

		// Record the byte length of this encoded ref.
		lastFileByteRanges = append(lastFileByteRanges, cw.n-before)
	}
	if lastFile != "" {
		fbr[lastFile] = lastFileByteRanges
	}
	return fbr, nil
}

func (s *fsUnitStore) String() string { return "fsUnitStore" }

// countingWriter wraps an io.Writer, counting the number of bytes
// written.
type countingWriter struct {
	io.Writer
	n int64
}

func (cr *countingWriter) Write(p []byte) (n int, err error) {
	n, err = cr.Writer.Write(p)
	cr.n += int64(n)
	return
}

// countingReader wraps an io.Reader, counting the number of bytes
// read.
type countingReader struct {
	io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
	n, err = cr.Reader.Read(p)
	cr.n += int64(n)
	return
}

func setCreateParentDirs(fs rwvfs.FileSystem) {
	type createParents interface {
		CreateParentDirs(bool)
	}
	if fs, ok := fs.(createParents); ok {
		fs.CreateParentDirs(true)
	}
}