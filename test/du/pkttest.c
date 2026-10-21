// O-RAN fronthaul-style AF_PACKET exercise, following OCUDU's OFH Ethernet
// transport (lib/ofh/ethernet): an eCPRI receive socket bound with
// SO_BINDTODEVICE, an IPPROTO_RAW send socket, SIOCSIFMTU to the current MTU,
// sendto() with a sockaddr_ll, and PACKET_QDISC_BYPASS.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <linux/if_packet.h>
#include <net/ethernet.h>
#include <net/if.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#define ETH_P_ECPRI 0xAEFE
#ifndef PACKET_QDISC_BYPASS
#define PACKET_QDISC_BYPASS 20
#endif

static int fails;
#define CHECK(c, ...) do { if (c) printf("ok   " __VA_ARGS__); else { fails++; printf("FAIL " __VA_ARGS__); printf(" (errno=%d %s)", errno, strerror(errno)); } printf("\n"); } while (0)

int main(int argc, char **argv) {
  const char *ifname = argc > 1 ? argv[1] : "fh0";
  int n = argc > 2 ? atoi(argv[2]) : 100;
  unsigned char peer[6] = {0x02, 0, 0, 0, 0, 0x02};

  int rx = socket(AF_PACKET, SOCK_RAW, htons(ETH_P_ECPRI));
  CHECK(rx >= 0, "socket(AF_PACKET, SOCK_RAW, ETH_P_ECPRI)");
  CHECK(setsockopt(rx, SOL_SOCKET, SO_BINDTODEVICE, ifname, strlen(ifname)) == 0, "SO_BINDTODEVICE %s", ifname);

  int tx = socket(AF_PACKET, SOCK_RAW | SOCK_NONBLOCK, IPPROTO_RAW);
  CHECK(tx >= 0, "socket(AF_PACKET, SOCK_RAW, IPPROTO_RAW)");
  struct ifreq ifr = {0};
  strncpy(ifr.ifr_name, ifname, IFNAMSIZ - 1);
  CHECK(ioctl(tx, SIOCGIFMTU, &ifr) == 0, "SIOCGIFMTU = %d", ifr.ifr_mtu);
  CHECK(ioctl(tx, SIOCSIFMTU, &ifr) == 0, "SIOCSIFMTU to the current MTU");
  ifr.ifr_mtu += 1;
  CHECK(ioctl(tx, SIOCSIFMTU, &ifr) < 0, "SIOCSIFMTU to another MTU fails");
  CHECK(ioctl(tx, SIOCGIFINDEX, &ifr) == 0, "SIOCGIFINDEX = %d", ifr.ifr_ifindex);
  int one = 1;
  CHECK(setsockopt(tx, SOL_PACKET, PACKET_QDISC_BYPASS, &one, sizeof one) == 0, "PACKET_QDISC_BYPASS");
  struct ifreq hw = {0};
  strncpy(hw.ifr_name, ifname, IFNAMSIZ - 1);
  ioctl(tx, SIOCGIFHWADDR, &hw);

  struct sockaddr_ll sll = {0};
  sll.sll_family = AF_PACKET;
  sll.sll_ifindex = ifr.ifr_ifindex;
  sll.sll_halen = ETH_ALEN;
  memcpy(sll.sll_addr, peer, 6);

  // Untagged and 802.1Q-tagged eCPRI frames, as OCUDU's frame builders make.
  unsigned char frame[64];
  int sent = 0, sent_vlan = 0;
  for (int i = 0; i < n; i++) {
    memset(frame, 0, sizeof frame);
    memcpy(frame, peer, 6);
    memcpy(frame + 6, hw.ifr_hwaddr.sa_data, 6);
    int off = 12;
    int vlan = i % 2;
    if (vlan) { frame[12] = 0x81; frame[13] = 0x00; frame[14] = 0x00; frame[15] = 0x64; off = 16; }
    frame[off] = 0xAE; frame[off + 1] = 0xFE;
    frame[off + 2] = 0x10; frame[off + 3] = 0x00;  // eCPRI header: rev 1, type 0 (IQ data)
    frame[off + 6] = i & 0xff;
    if (sendto(tx, frame, sizeof frame, 0, (struct sockaddr *)&sll, sizeof sll) == sizeof frame) {
      sent++;
      sent_vlan += vlan;
    }
  }
  CHECK(sent == n, "sendto %d/%d eCPRI frames (%d 802.1Q-tagged)", sent, n, sent_vlan);

  int got = 0;
  struct timespec t0, t1;
  clock_gettime(CLOCK_MONOTONIC, &t0);
  for (;;) {
    struct pollfd p = {rx, POLLIN, 0};
    if (poll(&p, 1, 3000) <= 0) break;
    unsigned char buf[2048];
    ssize_t r = recvfrom(rx, buf, sizeof buf, 0, NULL, NULL);
    if (r >= 14 && buf[12] == 0xAE && buf[13] == 0xFE) got++;
    if (got == n) break;
  }
  clock_gettime(CLOCK_MONOTONIC, &t1);
  CHECK(got == n, "received %d/%d eCPRI frames from the peer in %.1f ms", got, n,
        (t1.tv_sec - t0.tv_sec) * 1e3 + (t1.tv_nsec - t0.tv_nsec) / 1e6);
  printf("%s: %d failures\n", fails ? "FAILED" : "PASSED", fails);
  return fails != 0;
}
