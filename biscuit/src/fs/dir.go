package fs

import "fmt"
import "strings"
import "common"

const NAME_MAX    int = 512

var lhits=0

// allocation-less pathparts
type pathparts_t struct {
	path	string
	loc	int
}

func (pp *pathparts_t) pp_init(path string) {
	pp.path = path
	pp.loc = 0
}

func (pp *pathparts_t) next() (string, bool) {
	ret := ""
	for ret == "" {
		if pp.loc == len(pp.path) {
			return "", false
		}
		ret = pp.path[pp.loc:]
		nloc := strings.IndexByte(ret, '/')
		if nloc != -1 {
			ret = ret[:nloc]
			pp.loc += nloc + 1
		} else {
			pp.loc += len(ret)
		}
	}
	return ret, true
}

func sdirname(path string) (string, string) {
	fn := path
	l := len(fn)
	// strip all trailing slashes
	for i := l - 1; i >= 0; i-- {
		if fn[i] != '/' {
			break
		}
		fn = fn[:i]
		l--
	}
	s := ""
	for i := l - 1; i >= 0; i-- {
		if fn[i] == '/' {
			// remove the rightmost slash only if it is not the
			// first char (the root).
			if i == 0 {
				s = fn[0:1]
			} else {
				s = fn[:i]
			}
			fn = fn[i+1:]
			break
		}
	}

	return s, fn
}

func crname(path string, nilpatherr common.Err_t) (common.Err_t, bool) {
	if path == "" {
		return nilpatherr, false
	} else if path == "." || path == ".." {
		return -common.EINVAL, false
	}
	return 0, true
}

// directory data format
// 0-13,  file name characters
// 14-21, inode block/offset
// ...repeated, totaling 23 times
type dirdata_t struct {
	data	[]uint8
}

const(
	DNAMELEN = 14
	NDBYTES  = 22
	NDIRENTS = BSIZE/NDBYTES
)

func doffset(didx int, off int) int {
	if didx < 0 || didx >= NDIRENTS {
		panic("bad dirent index")
	}
	return NDBYTES*didx + off
}

func (dir *dirdata_t) filename(didx int) string {
	st := doffset(didx, 0)
	sl := dir.data[st : st + DNAMELEN]
	ret := make([]byte, 0, 14)
	for _, c := range sl {
		if c == 0 {
			break
		}
		ret = append(ret, c)
	}
	return string(ret)
}

func (dir *dirdata_t) inodenext(didx int) common.Inum_t {
	st := doffset(didx, 14)
	v := common.Readn(dir.data[:], 8, st)
	return common.Inum_t(v)
}

func (dir *dirdata_t) w_filename(didx int, fn string) {
	st := doffset(didx, 0)
	sl := dir.data[st : st + DNAMELEN]
	l := len(fn)
	for i := range sl {
		if i >= l {
			sl[i] = 0
		} else {
			sl[i] = fn[i]
		}
	}
}

func (dir *dirdata_t) w_inodenext(didx int, inum common.Inum_t) {
	st := doffset(didx, 14)
	common.Writen(dir.data[:], 8, st, int(inum))
}

type fdent_t struct {
	offset	int
	next	*fdent_t
}

// linked list of free directory entries
type fdelist_t struct {
	head	*fdent_t
	n	int
}

func (il *fdelist_t) addhead(off int) {
	d := &fdent_t{offset: off}
	d.next = il.head
	il.head = d
	il.n++
}

func (il *fdelist_t) remhead() (*fdent_t, bool) {
	var ret *fdent_t
	if il.head != nil {
		ret = il.head
		il.head = ret.next
		il.n--
	}
	return ret, ret != nil
}

func (il *fdelist_t) count() int {
	return il.n
}

// struct to hold the offset/priv of directory entry slots
type icdent_t struct {
	offset	int
	inum	common.Inum_t
}


// returns the offset of an empty directory entry. returns error if failed to
// allocate page for the new directory entry.
func (idm *imemnode_t) _denextempty() (int, common.Err_t) {
	dc := &idm.dentc
	if ret, ok := dc.freel.remhead(); ok {
		return ret.offset, 0
	}

	// see if we have an empty slot before expanding the directory
	if !idm.dentc.haveall {
		var de icdent_t
		found, err := idm._descan(func(fn string, tde icdent_t) bool {
			if fn == "" {
				de = tde
				return true
			}
			return false
		})
		if err != 0 {
			return 0, err
		}
		if found {
			return de.offset, 0
		}
	}

	// current dir blocks are full -- allocate new dirdata block
	newsz := idm.size + BSIZE
	b, err := idm.off2buf(idm.size, BSIZE, true, true, "_denextempty")
	if err != 0 {
		return 0, err
	}
	newoff := idm.size
	// start from 1 since we return slot 0 directly
	for i := 1; i < NDIRENTS; i++ {
		noff := newoff + NDBYTES*i
		idm._deaddempty(noff)  
	}
	
	b.Unlock()
	fslog.Write(b)  // log empty dir block, later writes absorpt it hopefully
	bcache.Relse(b, "_denextempty")
	
	idm.size = newsz
	return newoff, 0
}

// if _deinsert fails to allocate a page, idm is left unchanged.
func (idm *imemnode_t) _deinsert(name string, inum common.Inum_t) common.Err_t {
	// XXXPANIC
	//if _, err := idm._delookup(name); err == 0 {
	//	panic("dirent already exists")
	//}

	noff, err := idm._denextempty()
	if err != 0 {
		return err
	}
        // dennextempty() made the slot so we won't fill
	b, err := idm.off2buf(noff, NDBYTES, true, true, "_deinsert")
	if err != 0 {
		return err
	}
	ddata := dirdata_t{b.Data[noff%common.PGSIZE:]}

	ddata.w_filename(0, name)
	ddata.w_inodenext(0, inum)


	b.Unlock()
	fslog.Write(b)
	bcache.Relse(b, "_deinsert")
	
	icd := icdent_t{noff, inum}
	ok := idm._dceadd(name, icd)
	dc := &idm.dentc
	dc.haveall = dc.haveall && ok

	return 0
}

// calls f on each directory entry (including empty ones) until f returns true
// or it has been called on all directory entries. _descan returns true if f
// returned true.
func (idm *imemnode_t) _descan(f func(fn string, de icdent_t) bool) (bool, common.Err_t) {
	found := false
	for i := 0; i < idm.size; i+= BSIZE {
		b, err := idm.off2buf(i, BSIZE, false, true, "_descan")
		if err != 0 {
			return false, err
		}
		dd := dirdata_t{b.Data[:]}
		for j := 0; j < NDIRENTS; j++ {
			tfn := dd.filename(j)
			tpriv := dd.inodenext(j)
			tde := icdent_t{i+j*NDBYTES, tpriv}
			if f(tfn, tde) {
				found = true
				break
			}
		}
		b.Unlock()
		bcache.Relse(b, "_descan")
	}
	return found, 0
}

func (idm *imemnode_t) _delookup(fn string) (icdent_t, common.Err_t) {
	if fn == "" {
		panic("bad lookup")
	}
	if de, ok := idm.dentc.dents.lookup(fn); ok {
		return de, 0
	}
	var zi icdent_t
	if idm.dentc.haveall {
		// cache negative entries?
		return zi, -common.ENOENT
	}

	// not in cached dirents
	found := false
	haveall := true
	var de icdent_t
	_, err := idm._descan(func(tfn string, tde icdent_t) bool {
		if tfn == "" {
			return false
		}
		if tfn == fn {
			de = tde
			found = true
		}
		if !idm._dceadd(tfn, tde) {
			haveall = false
		}
		return found && !haveall
	})
	if err != 0 {
		return zi, err
	}
	idm.dentc.haveall = haveall
	if !found {
		return zi, -common.ENOENT
	}
	return de, 0
}

func (idm *imemnode_t) _deremove(fn string) (icdent_t, common.Err_t) {
	var zi icdent_t
	de, err := idm._delookup(fn)
	if err != 0 {
		return zi, err
	}

	b, err := idm.off2buf(de.offset, NDBYTES, true, true, "_deremove")
	if err != 0 {
		return zi, err
	}
	dirdata := dirdata_t{b.Data[de.offset%common.PGSIZE:]}
	dirdata.w_filename(0, "")
	dirdata.w_inodenext(0, common.Inum_t(0))
	b.Unlock()
	fslog.Write(b)
	bcache.Relse(b, "_deremove")
	// add back to free dents
	idm.dentc.dents.remove(fn)
	idm._deaddempty(de.offset)
	return de, 0
}

// returns the filename mapping to tnum
func (idm *imemnode_t) _denamefor(tnum common.Inum_t) (string, common.Err_t) {
	// check cache first
	var fn string
	found := idm.dentc.dents.iter(func(dn string, de icdent_t) bool {
		if de.inum == tnum {
			fn = dn
			return true
		}
		return false
	})
	if found {
		return fn, 0
	}

	// not in cache; shucks!
	var de icdent_t
	found, err := idm._descan(func(tfn string, tde icdent_t) bool {
		if tde.inum == tnum {
			fn = tfn
			de = tde
			return true
		}
		return false
	})
	if err != 0 {
		return "", err
	}
	if !found {
		return "", -common.ENOENT
	}
	idm._dceadd(fn, de)
	return fn, 0
}

// returns true if idm, a directory, is empty (excluding ".." and ".").
func (idm *imemnode_t) _deempty() (bool, common.Err_t) {
	if idm.dentc.haveall {
		dentc := &idm.dentc
		hasfiles := dentc.dents.iter(func(dn string, de icdent_t) bool {
			if dn != "." && dn != ".." {
				return true
			}
			return false
		})
		return !hasfiles, 0
	}
	notempty, err := idm._descan(func(fn string, de icdent_t) bool {
		return fn != "" && fn != "." && fn != ".."
	})
	if err != 0 {
		return false, err
	}
	return !notempty, 0
}

// empties the dirent cache, returning the number of dents purged.
func (idm *imemnode_t) _derelease() int {
	dc := &idm.dentc
	dc.haveall = false
	dc.dents.clear()
	dc.freel.head = nil
	ret := dc.max
	common.Syslimit.Dirents.Given(uint(ret))
	dc.max = 0
	return ret
}

// ensure that an insert/unlink cannot fail i.e. fail to allocate a page. if fn
// == "", look for an empty dirent, otherwise make sure the page containing fn
// is in the page cache.
func (idm *imemnode_t) _deprobe(fn string) (*common.Bdev_block_t, common.Err_t) {
	if fn != "" {
		de, err := idm._delookup(fn)
		if err != 0 {
			return nil, err
		}
		noff := de.offset
		b, err := idm.off2buf(noff, NDBYTES, true, true, "_deprobe_fn")
		b.Unlock()
		return b, err
	}
	noff, err := idm._denextempty()
	if err != 0 {
		return nil, err
	}
	b, err := idm.off2buf(noff, NDBYTES, true, true, "_deprobe_nil")
	if err != 0 {
		return nil, err
	}
	b.Unlock()
	idm._deaddempty(noff)
	return b, 0
}

// returns true if this idm has enough free cache space for a single dentry
func (idm *imemnode_t) _demayadd() bool {
	dc := &idm.dentc
	//have := len(dc.dents) + len(dc.freem)
	have := dc.dents.nodes + dc.freel.count()
	if have + 1 < dc.max {
		return true
	}
	// reserve more directory entries
	take := 64
	if common.Syslimit.Dirents.Taken(uint(take)) {
		dc.max += take
		return true
	}
	lhits++
	return false
}

// caching is best-effort. returns true if fn was added to the cache
func (idm *imemnode_t) _dceadd(fn string, de icdent_t) bool {
	dc := &idm.dentc
	if !idm._demayadd() {
		return false
	}
	dc.dents.insert(fn, de)
	return true
}

func (idm *imemnode_t) _deaddempty(off int) {
	if !idm._demayadd() {
		return
	}
	dc := &idm.dentc
	dc.freel.addhead(off)
}

// guarantee that there is enough memory to insert at least one directory
// entry.
func (idm *imemnode_t) probe_insert() (*common.Bdev_block_t, common.Err_t) {
	// insert and remove a fake directory entry, forcing a page allocation
	// if necessary.
	b, err := idm._deprobe("")
	if err != 0 {
		return nil, err
	}
	return b, 0
}

// guarantee that there is enough memory to unlink a dirent (which may require
// allocating a page to load the dirents from disk).
func (idm *imemnode_t) probe_unlink(fn string) (*common.Bdev_block_t, common.Err_t) {
	b, err := idm._deprobe(fn)
	if err != 0 {
		return nil, err
	}
	return b, 0
}


func (idm *imemnode_t) ilookup(name string) (common.Inum_t, common.Err_t) {
	// did someone confuse a file with a directory?
	if idm.itype != I_DIR {
		return 0, -common.ENOTDIR
	}
	de, err := idm._delookup(name)

	if err != 0 {
		return 0, err
	}
	return de.inum, 0
}

// creates a new directory entry with name "name" and inode number priv
func (idm *imemnode_t) iinsert(name string, inum common.Inum_t) common.Err_t {
	if idm.itype != I_DIR {
		return -common.ENOTDIR
	}
	if _, err := idm._delookup(name); err == 0 {
		return -common.EEXIST
	} else if err != -common.ENOENT {
		return err
	}
	if inum < 0 {
		fmt.Printf("insert: negative inum %v %v\n", name, inum)
		panic("iinsert")
	}
	err := idm._deinsert(name, inum)
	return err
}

// returns inode number of unliked inode so caller can decrement its ref count
func (idm *imemnode_t) iunlink(name string) (common.Inum_t, common.Err_t) {
	if idm.itype != I_DIR {
		panic("unlink to non-dir")
	}
	de, err := idm._deremove(name)
	if err != 0 {
		return 0, err
	}
	return de.inum, 0
}

// returns true if the inode has no directory entries
func (idm *imemnode_t) idirempty() (bool, common.Err_t) {
	if idm.itype != I_DIR {
		panic("i am not a dir")
	}
	return idm._deempty()
}
