// Exercises the hugepage interfaces DPDK's EAL uses.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/shm.h>
#include <sys/stat.h>
#include <sys/vfs.h>
#include <unistd.h>

#ifndef MFD_HUGETLB
#define MFD_HUGETLB 4
#endif
#ifndef MAP_HUGE_SHIFT
#define MAP_HUGE_SHIFT 26
#endif
#define HP (2UL << 20)
#define HUGETLBFS_MAGIC 0x958458f6

static int fails;
#define CHECK(c, ...) do { if (c) printf("ok   " __VA_ARGS__); else { fails++; printf("FAIL " __VA_ARGS__); printf("     errno=%d %s\n", errno, strerror(errno)); } printf("\n"); } while (0)

static void cat(const char *p, const char *grep) {
  char line[256];
  FILE *f = fopen(p, "r");
  if (!f) { printf("     cannot open %s: %s\n", p, strerror(errno)); fails++; return; }
  while (fgets(line, sizeof line, f))
    if (!grep || strstr(line, grep)) printf("     %s: %s", p, line);
  fclose(f);
}

int main(void) {
  cat("/proc/meminfo", "Huge");
  cat("/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages", NULL);
  cat("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages", NULL);

  // memfd_create(MFD_HUGETLB | MFD_HUGE_2MB), as DPDK --in-memory does.
  int fd = memfd_create("dpdk", MFD_HUGETLB | (21 << MAP_HUGE_SHIFT));
  CHECK(fd >= 0, "memfd_create(MFD_HUGETLB|MFD_HUGE_2MB)");
  CHECK(ftruncate(fd, 4096) < 0 && errno == EINVAL, "ftruncate(unaligned) = EINVAL");
  CHECK(ftruncate(fd, 4 * HP) == 0, "ftruncate(4 huge pages)");
  char *p = mmap(NULL, 4 * HP, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_POPULATE, fd, 0);
  CHECK(p != MAP_FAILED && ((unsigned long)p % HP) == 0, "mmap(memfd) huge-aligned %p", p);
  if (p != MAP_FAILED) { memset(p, 1, 4 * HP); }
  CHECK(mmap(NULL, HP, PROT_READ, MAP_SHARED, fd, 4096) == MAP_FAILED && errno == EINVAL, "mmap(unaligned offset) = EINVAL");
  cat("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages", NULL);
  CHECK(memfd_create("bad", MFD_HUGETLB | (30 << MAP_HUGE_SHIFT)) < 0 && errno == EINVAL, "memfd_create(MFD_HUGE_1GB) = EINVAL");

  // Anonymous MAP_HUGETLB.
  char *a = mmap(NULL, 3 * HP + 1, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_ANONYMOUS | MAP_HUGETLB, -1, 0);
  CHECK(a != MAP_FAILED && ((unsigned long)a % HP) == 0, "mmap(MAP_SHARED|MAP_ANONYMOUS|MAP_HUGETLB) %p", a);
  if (a != MAP_FAILED) a[3 * HP] = 1;  // length rounded up to 4 huge pages
  char *b = mmap(NULL, HP, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_HUGETLB, -1, 0);
  CHECK(b != MAP_FAILED && ((unsigned long)b % HP) == 0, "mmap(MAP_PRIVATE|MAP_ANONYMOUS|MAP_HUGETLB) %p", b);
  int tf = open("/proc/self/exe", O_RDONLY);
  CHECK(mmap(NULL, HP, PROT_READ, MAP_PRIVATE | MAP_HUGETLB, tf, 0) == MAP_FAILED && errno == EINVAL, "mmap(MAP_HUGETLB, non-hugetlbfs fd) = EINVAL");

  // SysV shm.
  int id = shmget(IPC_PRIVATE, HP, IPC_CREAT | SHM_HUGETLB | 0600);
  CHECK(id >= 0, "shmget(SHM_HUGETLB)");
  if (id >= 0) {
    char *s = shmat(id, NULL, 0);
    CHECK(s != (void *)-1 && ((unsigned long)s % HP) == 0, "shmat huge-aligned %p", s);
    shmctl(id, IPC_RMID, NULL);
  }

  // hugetlbfs mount, as DPDK's default (file-per-page) mode uses.
  mkdir("/tmp/huge", 0755);
  CHECK(mount("nodev", "/tmp/huge", "hugetlbfs", 0, "pagesize=2M,size=8M") == 0, "mount -t hugetlbfs");
  struct statfs st;
  CHECK(statfs("/tmp/huge", &st) == 0 && st.f_type == HUGETLBFS_MAGIC && st.f_bsize == HP, "statfs: type=%lx bsize=%ld blocks=%ld free=%ld", (long)st.f_type, (long)st.f_bsize, (long)st.f_blocks, (long)st.f_bfree);
  cat("/proc/mounts", "hugetlbfs");
  int hf = open("/tmp/huge/rtemap_0", O_CREAT | O_RDWR, 0600);
  CHECK(hf >= 0, "open hugetlbfs file");
  CHECK(fallocate(hf, 0, 0, 1) == 0, "fallocate(1 byte) rounds to a huge page");
  struct stat fst;
  fstat(hf, &fst);
  printf("     size after fallocate: %ld\n", (long)fst.st_size);
  char *h = mmap(NULL, HP, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_POPULATE, hf, 0);
  CHECK(h != MAP_FAILED, "mmap hugetlbfs file");
  if (h != MAP_FAILED) h[0] = 1;
  CHECK(mmap(NULL, 6 * HP, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_POPULATE, hf, 0) == MAP_FAILED || 1, "mmap beyond size= (informational)");
  // Exhaust the 8M mount limit: 4 pages total.
  int full = open("/tmp/huge/big", O_CREAT | O_RDWR, 0600);
  CHECK(fallocate(full, 0, 0, 4 * HP) < 0 && errno == ENOSPC, "fallocate beyond size= fails with ENOSPC");
  cat("/proc/meminfo", "HugePages_Free");
  printf("%s: %d failures\n", fails ? "FAILED" : "PASSED", fails);
  return fails != 0;
}
