// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tmpfs

import (
	"fmt"
	"math"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/hugetlb"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// HugetlbfsName is the name of the hugetlbfs filesystem type.
const HugetlbfsName = "hugetlbfs"

// smallPagesPerHugePage is the number of small pages in a huge page.
const smallPagesPerHugePage = hostarch.HugePageSize / hostarch.PageSize

// Mount options for hugetlbfs, in addition to tmpfs's nr_inodes, mode, uid
// and gid.
const (
	optPageSize = "pagesize"
	optMinSize  = "min_size"
)

// HugetlbfsFilesystemType implements vfs.FilesystemType for hugetlbfs: a
// tmpfs whose files allocate from the sandbox's hugetlb.Pool, whose sizes and
// mappings are rounded to huge pages, and which reports itself as hugetlbfs
// in /proc/mounts and statfs(2), as applications such as DPDK require.
//
// +stateify savable
type HugetlbfsFilesystemType struct{}

// Name implements vfs.FilesystemType.Name.
func (HugetlbfsFilesystemType) Name() string {
	return HugetlbfsName
}

// Release implements vfs.FilesystemType.Release.
func (HugetlbfsFilesystemType) Release(ctx context.Context) {}

// GetFilesystem implements vfs.FilesystemType.GetFilesystem.
func (fstype HugetlbfsFilesystemType) GetFilesystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, source string, opts vfs.GetFilesystemOptions) (*vfs.Filesystem, *vfs.Dentry, error) {
	pool := hugetlb.PoolFromContext(ctx)
	var (
		tmpfsData   []string
		printedOpts = []string{fmt.Sprintf("pagesize=%dM", hostarch.HugePageSize>>20)}
	)
	// See fs/hugetlbfs/inode.c:hugetlbfs_parse_param().
	for _, opt := range vfs.GenericParseMountOptionsOrdered(opts.Data) {
		switch opt.Key {
		case optPageSize:
			size, _, err := parseSize(opt.Value)
			if err != nil || size != hostarch.HugePageSize {
				ctx.Warningf("hugetlbfs: unsupported page size %q; only %d bytes is supported", opt.Value, hostarch.HugePageSize)
				return nil, nil, linuxerr.EINVAL
			}
		case optSize:
			size, err := parseHugetlbfsSize(opt.Value, pool)
			if err != nil {
				ctx.Debugf("hugetlbfs: invalid size %q: %v", opt.Value, err)
				return nil, nil, linuxerr.EINVAL
			}
			tmpfsData = append(tmpfsData, fmt.Sprintf("%s=%d", optSize, size))
			printedOpts = append(printedOpts, fmt.Sprintf("size=%d", size))
		case optMinSize:
			// Reserving pages up front is not supported; the pool is only
			// an accounting limit.
			if _, err := parseHugetlbfsSize(opt.Value, pool); err != nil {
				return nil, nil, linuxerr.EINVAL
			}
		case optNrInodes, optMode, optUID, optGID:
			tmpfsData = append(tmpfsData, opt.Key+"="+opt.Value)
			printedOpts = append(printedOpts, opt.Key+"="+opt.Value)
		default:
			ctx.Warningf("hugetlbfs: unknown option: %s", opt.Key)
			return nil, nil, linuxerr.EINVAL
		}
	}
	tmpfsOpts := FilesystemOpts{
		FilesystemType: fstype,
		// Like Linux, hugetlbfs is unlimited by default other than by the
		// pool.
		DisableDefaultSizeLimit: true,
	}
	if internal, ok := opts.InternalData.(FilesystemOpts); ok {
		tmpfsOpts.Usage = internal.Usage
		tmpfsOpts.MemoryFile = internal.MemoryFile
	}
	vfsfs, root, err := FilesystemType{}.GetFilesystem(ctx, vfsObj, creds, source, vfs.GetFilesystemOptions{
		Data:          strings.Join(tmpfsData, ","),
		InternalData:  tmpfsOpts,
		InternalMount: opts.InternalMount,
	})
	if err != nil {
		return nil, nil, err
	}
	fs := vfsfs.Impl().(*filesystem)
	fs.hugetlbPool = pool
	fs.mopts = strings.Join(printedOpts, ",")
	return vfsfs, root, nil
}

// parseHugetlbfsSize parses a hugetlbfs size option, which may be a
// percentage of the pool, and rounds it down to huge pages.
func parseHugetlbfsSize(value string, pool *hugetlb.Pool) (uint64, error) {
	var size uint64
	if pct, ok := strings.CutSuffix(value, "%"); ok {
		n, _, err := parseSize(pct)
		if err != nil {
			return 0, err
		}
		size = pool.TotalPages() * hostarch.HugePageSize * n / 100
	} else {
		var err error
		if size, _, err = parseSize(value); err != nil {
			return 0, err
		}
	}
	return size &^ (hostarch.HugePageSize - 1), nil
}

// isHugetlb returns whether fs is a hugetlbfs filesystem.
func (fs *filesystem) isHugetlb() bool {
	return fs.hugetlbPool != nil
}

// IsHugetlbFile returns whether fd is a file on a hugetlbfs filesystem, as for
// Linux's is_file_hugepages().
func IsHugetlbFile(fd *vfs.FileDescription) bool {
	rfd, ok := fd.Impl().(*regularFileFD)
	return ok && rfd.inode().fs.isHugetlb()
}

// hugetlbStatFS returns statfs(2) for a hugetlbfs filesystem, in units of
// huge pages, as fs/hugetlbfs/inode.c:hugetlbfs_statfs() does.
func (fs *filesystem) hugetlbStatFS() linux.Statfs {
	st := linux.Statfs{
		Type:         linux.HUGETLBFS_MAGIC,
		BlockSize:    hostarch.HugePageSize,
		FragmentSize: hostarch.HugePageSize,
		NameLength:   linux.NAME_MAX,
	}
	// Without a size limit, Linux reports the pool's size.
	blocks := fs.hugetlbPool.TotalPages()
	if fs.maxSizeInPages < math.MaxInt64/hostarch.PageSize {
		blocks = min(blocks, fs.maxSizeInPages/smallPagesPerHugePage)
	}
	used := (fs.pagesUsed.Load() + smallPagesPerHugePage - 1) / smallPagesPerHugePage
	free := min(fs.hugetlbPool.FreePages(), blocks-min(used, blocks))
	st.Blocks = blocks
	st.BlocksFree = free
	st.BlocksAvailable = free
	st.Files = fs.maxInodes
	st.FilesFree = fs.maxInodes - min(fs.inodesUsed.Load(), fs.maxInodes)
	return st
}
