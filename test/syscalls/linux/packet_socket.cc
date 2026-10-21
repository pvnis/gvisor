// Copyright 2021 The gVisor Authors.
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

#include <net/if.h>
#include <netinet/if_ether.h>
#include <netpacket/packet.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/types.h>

#include <algorithm>
#include <cerrno>
#include <iterator>
#include <limits>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/syscalls/linux/ip_socket_test_util.h"
#include "test/util/capability_util.h"
#include "test/util/file_descriptor.h"
#include "test/util/logging.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/socket_util.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {

namespace {

using ::testing::AnyOf;
using ::testing::Combine;
using ::testing::Eq;
using ::testing::Values;

#ifndef __Fuchsia__  // Fuchsia doesn't support caps.
TEST(PacketSocketNoCapTest, CreationFailsWithEPERM) {
  AutoCapability cap(CAP_NET_RAW, false);
  ASSERT_THAT(socket(AF_PACKET, SOCK_DGRAM, htons(ETH_P_ALL)),
              SyscallFailsWithErrno(EPERM));

  // Fork to avoid changing the user namespace of the original test process.
  EXPECT_THAT(InForkedProcess([&] {
                // Fails with EPERM because we don't have CAP_NET_RAW.
                TEST_CHECK_ERRNO(syscall(SYS_socket, AF_PACKET, SOCK_DGRAM,
                                         htons(ETH_P_ALL)),
                                 EPERM);
                // Enter a new userns to gain all caps in the new userns.
                TEST_CHECK_SUCCESS(syscall(SYS_unshare, CLONE_NEWUSER));
                // But socket() should still fails with EPERM, for we don't have
                // CAP_NET_RAW in the original userns (that owns the netns we
                // are in).
                TEST_CHECK_ERRNO(syscall(SYS_socket, AF_PACKET, SOCK_DGRAM,
                                         htons(ETH_P_ALL)),
                                 EPERM);
                _exit(0);
              }),
              IsPosixErrorOkAndHolds(0));
}
#endif  // __Fuchsia__

class PacketSocketCreationTest
    : public ::testing::TestWithParam<std::tuple<int, int>> {
 protected:
  void SetUp() override {
    const auto [type, protocol] = GetParam();
    SKIP_IF(
        !ASSERT_NO_ERRNO_AND_VALUE(HavePacketSocketCapability(type, protocol)));
  }
};

TEST_P(PacketSocketCreationTest, Create) {
  const auto [type, protocol] = GetParam();
  FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Socket(AF_PACKET, type, htons(protocol)));
  EXPECT_GE(fd.get(), 0);
}

INSTANTIATE_TEST_SUITE_P(AllPacketSocketTests, PacketSocketCreationTest,
                         Combine(Values(SOCK_DGRAM, SOCK_RAW),
                                 Values(0, 1, 255, ETH_P_IP, ETH_P_IPV6,
                                        std::numeric_limits<uint16_t>::max())));

class PacketSocketTest : public ::testing::TestWithParam<int> {
 protected:
  void SetUp() override {
    // The tests need packet sockets with ETH_P_IP, even though socket_ is not
    // bound to any protocol.
    SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(
        HavePacketSocketCapability(GetParam(), ETH_P_IP)));

    socket_ = ASSERT_NO_ERRNO_AND_VALUE(Socket(AF_PACKET, GetParam(), 0));
  }

  FileDescriptor socket_;
};

TEST_P(PacketSocketTest, GetSockName) {
  {
    // First check the local address of an unbound packet socket.
    sockaddr_ll addr;
    socklen_t addrlen = sizeof(addr);
    ASSERT_THAT(getsockname(socket_.get(), reinterpret_cast<sockaddr*>(&addr),
                            &addrlen),
                SyscallSucceeds());
    // sockaddr_ll ends with an 8 byte physical address field, but only the
    // bytes that are used in the sockaddr_ll.sll_addr field are included in the
    // address length. Seems Linux used to return the size of sockaddr_ll, but
    // https://github.com/torvalds/linux/commit/0fb375fb9b93b7d822debc6a734052337ccfdb1f
    // changed things to only return `sizeof(sockaddr_ll) + sll.sll_addr`.
    ASSERT_THAT(addrlen, AnyOf(Eq(sizeof(addr)),
                               Eq(sizeof(addr) - sizeof(addr.sll_addr))));
    EXPECT_EQ(addr.sll_family, AF_PACKET);
    EXPECT_EQ(addr.sll_ifindex, 0);

    if (IsRunningOnGvisor() && !IsRunningWithHostinet() &&
        GvisorPlatform() != Platform::kFuchsia) {
      // TODO(https://gvisor.dev/issue/6530): Do not assume all interfaces have
      // an ethernet address.
      EXPECT_EQ(addr.sll_halen, ETH_ALEN);
    } else {
      EXPECT_EQ(addr.sll_halen, 0);
    }
    EXPECT_EQ(ntohs(addr.sll_protocol), 0);
    EXPECT_EQ(addr.sll_hatype, 0);
  }
  // Next we bind the socket to loopback before checking the local address.
  const sockaddr_ll bind_addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(ETH_P_IP),
      .sll_ifindex = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex()),
  };
  ASSERT_THAT(bind(socket_.get(), reinterpret_cast<const sockaddr*>(&bind_addr),
                   sizeof(bind_addr)),
              SyscallSucceeds());
  {
    sockaddr_ll addr;
    socklen_t addrlen = sizeof(addr);
    ASSERT_THAT(getsockname(socket_.get(), reinterpret_cast<sockaddr*>(&addr),
                            &addrlen),
                SyscallSucceeds());
    ASSERT_THAT(addrlen,
                AnyOf(Eq(sizeof(addr)),
                      Eq(sizeof(addr) - sizeof(addr.sll_addr) + ETH_ALEN)));
    EXPECT_EQ(addr.sll_family, AF_PACKET);
    EXPECT_EQ(addr.sll_ifindex, bind_addr.sll_ifindex);
    EXPECT_EQ(addr.sll_halen, ETH_ALEN);
    // Bound to loopback which has the all zeroes address.
    for (int i = 0; i < addr.sll_halen; ++i) {
      EXPECT_EQ(addr.sll_addr[i], 0) << "byte mismatch @ idx = " << i;
    }
    EXPECT_EQ(ntohs(addr.sll_protocol), htons(addr.sll_protocol));
    if (IsRunningOnGvisor() && !IsRunningWithHostinet() &&
        GvisorPlatform() != Platform::kFuchsia) {
      // TODO(https://gvisor.dev/issue/6621): Support populating sll_hatype.
      EXPECT_EQ(addr.sll_hatype, 0);
    } else {
      EXPECT_EQ(addr.sll_hatype, ARPHRD_LOOPBACK);
    }
  }
}

// Expects that a packet can be read from the given packet socket that contains
// a UDP packet whose source and destination port match the one in the given
// address, and whose payload matches the expected payload.
//
// On success, writes the link-layer source address for the packet into
// the provided output parameter (if it is non-null).
void ExpectReceiveOnPacketSocket(FileDescriptor& socket,
                                 bool ethernet_header_included,
                                 const sockaddr_in& expected_udp_addr,
                                 const uint64_t expected_udp_payload,
                                 sockaddr_ll* src_out = nullptr) {
  // Declare each section of the packet as a separate stack variable in order
  // to ensure all sections are 8-byte aligned.
  ethhdr eth;
  iphdr ip;
  udphdr udp;
  uint64_t payload;
  char unused;

  constexpr size_t kStorageLen =
      sizeof(eth) + sizeof(ip) + sizeof(udp) + sizeof(payload) + sizeof(unused);
  char storage[kStorageLen];

  sockaddr_ll src;
  socklen_t src_len = sizeof(src);

  char* buf = storage;
  size_t buflen = kStorageLen;
  auto advance_buf = [&buf, &buflen](size_t amount) {
    buf += amount;
    buflen -= amount;
  };
  size_t expected_read_len = buflen - sizeof(unused);
  if (!ethernet_header_included) {
    advance_buf(sizeof(eth));
    expected_read_len -= sizeof(eth);
  }

  iovec received_iov = {
      .iov_base = buf,
      .iov_len = buflen,
  };
  msghdr received_msg = {
      .msg_name = &src,
      .msg_namelen = src_len,
      .msg_iov = &received_iov,
      .msg_iovlen = 1,
  };

  // NB: poll indefinitely on Fuchsia to avoid timing out in Infra.
  int timeout = GvisorPlatform() == Platform::kFuchsia ? -1 : 1;
  ASSERT_THAT(RecvMsgTimeout(socket.get(), &received_msg, timeout),
              IsPosixErrorOkAndHolds(expected_read_len));

  // sockaddr_ll ends with an 8 byte physical address field, but ethernet
  // addresses only use 6 bytes. Linux used to return sizeof(sockaddr_ll)-2
  // here, but returns sizeof(sockaddr_ll) since
  // https://github.com/torvalds/linux/commit/b2cf86e1563e33a14a1c69b3e508d15dc12f804c.
  ASSERT_THAT(received_msg.msg_namelen,
              AnyOf(Eq(sizeof(src)),
                    Eq(sizeof(src) - sizeof(src.sll_addr) + ETH_ALEN)));
  EXPECT_EQ(src.sll_family, AF_PACKET);
  EXPECT_EQ(src.sll_ifindex, ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex()));
  EXPECT_EQ(src.sll_halen, ETH_ALEN);
  EXPECT_EQ(ntohs(src.sll_protocol), ETH_P_IP);
  // This came from the loopback device, so the address is all 0s.
  constexpr uint8_t allZeroesMAC[ETH_ALEN] = {};
  EXPECT_EQ(memcmp(src.sll_addr, allZeroesMAC, sizeof(allZeroesMAC)), 0);

  if (ethernet_header_included) {
    memcpy(&eth, buf, sizeof(eth));
    EXPECT_EQ(memcmp(eth.h_dest, allZeroesMAC, sizeof(allZeroesMAC)), 0);
    EXPECT_EQ(memcmp(eth.h_source, allZeroesMAC, sizeof(allZeroesMAC)), 0);
    EXPECT_EQ(ntohs(eth.h_proto), ETH_P_IP);
    advance_buf(sizeof(eth));
  }

  // IHL hold the size of the header in 4 byte units.
  memcpy(&ip, buf, sizeof(ip));
  EXPECT_EQ(ip.ihl, sizeof(iphdr) / 4);
  EXPECT_EQ(ip.version, IPVERSION);
  const uint16_t ip_pkt_size = sizeof(ip) + sizeof(udp) + sizeof(payload);
  EXPECT_EQ(ntohs(ip.tot_len), ip_pkt_size);
  EXPECT_EQ(ip.protocol, IPPROTO_UDP);
  EXPECT_EQ(ntohl(ip.daddr), INADDR_LOOPBACK);
  EXPECT_EQ(ntohl(ip.saddr), INADDR_LOOPBACK);
  advance_buf(sizeof(ip));

  memcpy(&udp, buf, sizeof(udp));
  EXPECT_EQ(udp.source, expected_udp_addr.sin_port);
  EXPECT_EQ(udp.dest, expected_udp_addr.sin_port);
  EXPECT_EQ(ntohs(udp.len), ip_pkt_size - sizeof(ip));
  advance_buf(sizeof(udp));

  memcpy(&payload, buf, sizeof(payload));
  EXPECT_EQ(payload, expected_udp_payload);
  if (src_out) {
    *src_out = src;
  }
}

TEST_P(PacketSocketTest, RebindProtocol) {
  const bool kEthHdrIncluded = GetParam() == SOCK_RAW;

  sockaddr_in udp_bind_addr = {
      .sin_family = AF_INET,
      .sin_addr = {.s_addr = htonl(INADDR_LOOPBACK)},
  };

  FileDescriptor udp_sock =
      ASSERT_NO_ERRNO_AND_VALUE(Socket(AF_INET, SOCK_DGRAM, 0));
  {
    // Bind the socket so that we have something to send packets to.
    //
    // If we didn't do this, the UDP packets we send will be responded to with
    // ICMP Destination Port Unreachable errors.
    ASSERT_THAT(
        bind(udp_sock.get(), reinterpret_cast<const sockaddr*>(&udp_bind_addr),
             sizeof(udp_bind_addr)),
        SyscallSucceeds());
    socklen_t addrlen = sizeof(udp_bind_addr);
    ASSERT_THAT(
        getsockname(udp_sock.get(), reinterpret_cast<sockaddr*>(&udp_bind_addr),
                    &addrlen),
        SyscallSucceeds());
    ASSERT_THAT(addrlen, sizeof(udp_bind_addr));
  }

  const int loopback_index = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex());

  auto send_udp_message = [&](const uint64_t v) {
    ASSERT_THAT(
        sendto(udp_sock.get(), reinterpret_cast<const char*>(&v), sizeof(v),
               0 /* flags */, reinterpret_cast<const sockaddr*>(&udp_bind_addr),
               sizeof(udp_bind_addr)),
        SyscallSucceeds());

    // Make sure the payload has been delivered (in case of asynchronous
    // delivery).
    char buf[sizeof(v)];
    // NB: poll indefinitely on Fuchsia to avoid timing out in Infra.
    int timeout = GvisorPlatform() == Platform::kFuchsia ? -1 : 1;
    EXPECT_THAT(RecvTimeout(udp_sock.get(), buf, sizeof(v), timeout),
                IsPosixErrorOkAndHolds(sizeof(v)));
    ASSERT_EQ(*reinterpret_cast<uint64_t*>(buf), v);
  };

  auto bind_to_network_protocol = [&](uint16_t protocol) {
    const sockaddr_ll packet_bind_addr = {
        .sll_family = AF_PACKET,
        .sll_protocol = htons(protocol),
        .sll_ifindex = loopback_index,
    };

    ASSERT_THAT(bind(socket_.get(),
                     reinterpret_cast<const sockaddr*>(&packet_bind_addr),
                     sizeof(packet_bind_addr)),
                SyscallSucceeds());
  };

  // The packet socket is not bound to IPv4 so we should not receive the sent
  // message.
  uint64_t counter = 0;
  ASSERT_NO_FATAL_FAILURE(send_udp_message(++counter));

  // Bind to IPv4 and expect to receive the UDP packet we send after binding.
  ASSERT_NO_FATAL_FAILURE(bind_to_network_protocol(ETH_P_IP));
  ASSERT_NO_FATAL_FAILURE(send_udp_message(++counter));
  ASSERT_NO_FATAL_FAILURE(ExpectReceiveOnPacketSocket(socket_, kEthHdrIncluded,
                                                      udp_bind_addr, counter));

  // Bind the packet socket to a random protocol.
  ASSERT_NO_FATAL_FAILURE(bind_to_network_protocol(255));
  ASSERT_NO_FATAL_FAILURE(send_udp_message(++counter));

  // Bind back to IPv4 and expect to the UDP packet we send after binding
  // back to IPv4.
  ASSERT_NO_FATAL_FAILURE(bind_to_network_protocol(ETH_P_IP));
  ASSERT_NO_FATAL_FAILURE(send_udp_message(++counter));
  ASSERT_NO_FATAL_FAILURE(ExpectReceiveOnPacketSocket(socket_, kEthHdrIncluded,
                                                      udp_bind_addr, counter));

  // A zero valued protocol number should not change the bound network protocol.
  ASSERT_NO_FATAL_FAILURE(bind_to_network_protocol(0));
  ASSERT_NO_FATAL_FAILURE(send_udp_message(++counter));
  ASSERT_NO_FATAL_FAILURE(ExpectReceiveOnPacketSocket(socket_, kEthHdrIncluded,
                                                      udp_bind_addr, counter));
}

// Receive sent frames when bound to ETH_P_ALL.
TEST_P(PacketSocketTest, ReceiveSentBoundToProtocolAll) {
  // If a packet socket is bound to the loopback interface with protocol
  // ETH_P_ALL, it should receive a frame that is sent twice: once on sending
  // and again on reception.

  sockaddr_in udp_bind_addr = {
      .sin_family = AF_INET,
      .sin_addr = {.s_addr = htonl(INADDR_LOOPBACK)},
  };

  FileDescriptor udp_sock =
      ASSERT_NO_ERRNO_AND_VALUE(Socket(AF_INET, SOCK_DGRAM, 0));
  {
    // Bind the socket so that we have something to send packets to.
    //
    // If we didn't do this, the UDP packets we send will be responded to with
    // ICMP Destination Port Unreachable errors.
    ASSERT_THAT(
        bind(udp_sock.get(), reinterpret_cast<const sockaddr*>(&udp_bind_addr),
             sizeof(udp_bind_addr)),
        SyscallSucceeds());
    socklen_t addrlen = sizeof(udp_bind_addr);
    ASSERT_THAT(
        getsockname(udp_sock.get(), reinterpret_cast<sockaddr*>(&udp_bind_addr),
                    &addrlen),
        SyscallSucceeds());
    ASSERT_THAT(addrlen, sizeof(udp_bind_addr));
    ASSERT_NE(udp_bind_addr.sin_port, 0);
  }

  // Bind the packet socket to the loopback interface with ETH_P_ALL.
  const struct sockaddr_ll bind_addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(ETH_P_ALL),
      .sll_ifindex = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex()),
      .sll_halen = ETH_ALEN,
  };
  ASSERT_THAT(bind(socket_.get(), reinterpret_cast<const sockaddr*>(&bind_addr),
                   sizeof(bind_addr)),
              SyscallSucceeds());

  const uint64_t kContents = 0xAAAAAAAAAAAAAAAA;

  // Send to the same UDP socket so we don't interfere with other tests.
  ASSERT_THAT(sendto(udp_sock.get(), &kContents, sizeof(kContents), 0,
                     reinterpret_cast<const struct sockaddr*>(&udp_bind_addr),
                     sizeof(udp_bind_addr)),
              SyscallSucceeds());

  const bool kExpectEthernetHeader = GetParam() == SOCK_RAW;
  sockaddr_ll src_addr;

  // Receive the outgoing frame.
  ExpectReceiveOnPacketSocket(socket_, kExpectEthernetHeader, udp_bind_addr,
                              kContents, &src_addr);
  ASSERT_EQ(src_addr.sll_pkttype, PACKET_OUTGOING);

  // Then receive the incoming frame.
  ExpectReceiveOnPacketSocket(socket_, kExpectEthernetHeader, udp_bind_addr,
                              kContents, &src_addr);
  ASSERT_EQ(src_addr.sll_pkttype, PACKET_HOST);
}

// Verify that the ifindex field of sockaddr_ll must be provided when calling
// sendto on a packet socket (e.g. the interface cannot be inferred from the
// bound address).
TEST_P(PacketSocketTest, SendWithoutTargetDevice) {
  if (IsRunningOnGvisor() && GvisorPlatform() != Platform::kFuchsia) {
    GTEST_SKIP() << "gVisor does not support sending on packet sockets.";
  }

  const uint16_t kArbitraryProtocol = 0xFFFF;

  // Bind the packet socket to the loopback interface. Despite this, subsequent
  // sends will require the interface be specified.
  const struct sockaddr_ll bind_addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(kArbitraryProtocol),
      .sll_ifindex = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex()),
      .sll_halen = ETH_ALEN,
  };
  ASSERT_THAT(bind(socket_.get(), reinterpret_cast<const sockaddr*>(&bind_addr),
                   sizeof(bind_addr)),
              SyscallSucceeds());

  // Prepare a send buffer with a valid Ethernet header.
  // This enables sending with SOCK_RAW, and is inconsequential for SOCK_DGRAM.
  const uint64_t kContents = 0xAAAAAAAAAAAAAAAA;
  struct ethhdr eth = {};
  eth.h_proto = htons(kArbitraryProtocol);
  char send_buf[sizeof(eth) + sizeof(kContents)];
  memcpy(send_buf, &eth, sizeof(eth));
  memcpy(send_buf + sizeof(ethhdr), &kContents, sizeof(kContents));

  // Send to a valid address.
  const struct sockaddr_ll valid_addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(kArbitraryProtocol),
      .sll_ifindex = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex()),
      .sll_halen = ETH_ALEN,
  };
  ASSERT_THAT(sendto(socket_.get(), send_buf, sizeof(send_buf), 0,
                     reinterpret_cast<const struct sockaddr*>(&valid_addr),
                     sizeof(valid_addr)),
              SyscallSucceedsWithValue(sizeof(send_buf)));

  // Attempt to send without specifying the interface.
  const struct sockaddr_ll no_dev_addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(kArbitraryProtocol),
      .sll_ifindex = 0,
      .sll_halen = ETH_ALEN,
  };
  ASSERT_THAT(sendto(socket_.get(), send_buf, sizeof(send_buf), 0,
                     reinterpret_cast<const struct sockaddr*>(&no_dev_addr),
                     sizeof(no_dev_addr)),
              SyscallFailsWithErrno(ENXIO));
}

INSTANTIATE_TEST_SUITE_P(AllPacketSocketTests, PacketSocketTest,
                         Values(SOCK_DGRAM, SOCK_RAW));

// Tests that a PACKET_FANOUT_HASH group delivers each frame to exactly one
// of its sockets, and keeps a flow on one socket.
TEST(PacketSocketFanoutTest, HashDeliversEachFrameOnce) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HavePacketSocketCapability(SOCK_RAW, 0)));
  constexpr uint16_t kEtherType = 0x88B5;  // IEEE local experimental.
  constexpr int kMembers = 2;
  constexpr int kFlows = 16;
  constexpr int kFramesPerFlow = 4;
  const int lo = ASSERT_NO_ERRNO_AND_VALUE(GetLoopbackIndex());
  sockaddr_ll addr = {
      .sll_family = AF_PACKET,
      .sll_protocol = htons(kEtherType),
      .sll_ifindex = lo,
      .sll_halen = ETH_ALEN,
  };

  FileDescriptor members[kMembers];
  // A group ID unlikely to collide with other users of the network namespace.
  constexpr int kFanoutHash = 0;  // PACKET_FANOUT_HASH, from <linux/if_packet.h>.
  const int fanout_arg = (getpid() & 0xffff) | (kFanoutHash << 16);
  for (auto& member : members) {
    member = ASSERT_NO_ERRNO_AND_VALUE(
        Socket(AF_PACKET, SOCK_RAW | SOCK_NONBLOCK, htons(kEtherType)));
    ASSERT_THAT(
        bind(member.get(), reinterpret_cast<sockaddr*>(&addr), sizeof(addr)),
        SyscallSucceeds());
    ASSERT_THAT(setsockopt(member.get(), SOL_PACKET, PACKET_FANOUT, &fanout_arg,
                           sizeof(fanout_arg)),
                SyscallSucceeds());
  }
  // Joining twice fails.
  EXPECT_THAT(setsockopt(members[0].get(), SOL_PACKET, PACKET_FANOUT,
                         &fanout_arg, sizeof(fanout_arg)),
              SyscallFailsWithErrno(EALREADY));

  FileDescriptor tx = ASSERT_NO_ERRNO_AND_VALUE(Socket(AF_PACKET, SOCK_RAW, 0));
  for (int i = 0; i < kFlows * kFramesPerFlow; i++) {
    char frame[ETH_ZLEN] = {};
    ethhdr* eth = reinterpret_cast<ethhdr*>(frame);
    eth->h_source[5] = i % kFlows;  // The flow.
    eth->h_proto = htons(kEtherType);
    ASSERT_THAT(sendto(tx.get(), frame, sizeof(frame), 0,
                       reinterpret_cast<sockaddr*>(&addr), sizeof(addr)),
                SyscallSucceedsWithValue(sizeof(frame)));
  }

  // Collect which member received each flow's frames.
  int flow_member[kFlows];
  int flow_frames[kFlows] = {};
  std::fill(std::begin(flow_member), std::end(flow_member), -1);
  int total = 0;
  const absl::Time deadline = absl::Now() + absl::Seconds(5);
  while (total < kFlows * kFramesPerFlow && absl::Now() < deadline) {
    for (int m = 0; m < kMembers; m++) {
      char buf[ETH_FRAME_LEN];
      int n = recv(members[m].get(), buf, sizeof(buf), 0);
      if (n < 0) {
        ASSERT_EQ(errno, EAGAIN);
        continue;
      }
      ASSERT_GE(n, static_cast<int>(sizeof(ethhdr)));
      int flow = reinterpret_cast<ethhdr*>(buf)->h_source[5];
      ASSERT_LT(flow, kFlows);
      if (flow_member[flow] == -1) {
        flow_member[flow] = m;
      }
      EXPECT_EQ(flow_member[flow], m) << "flow " << flow << " split";
      flow_frames[flow]++;
      total++;
    }
    absl::SleepFor(absl::Milliseconds(1));
  }
  EXPECT_EQ(total, kFlows * kFramesPerFlow);
  for (int f = 0; f < kFlows; f++) {
    EXPECT_EQ(flow_frames[f], kFramesPerFlow) << "flow " << f;
  }
  // No frame was delivered to more than one member.
  absl::SleepFor(absl::Milliseconds(50));
  for (auto& member : members) {
    char buf[ETH_FRAME_LEN];
    EXPECT_THAT(recv(member.get(), buf, sizeof(buf), 0),
                SyscallFailsWithErrno(EAGAIN));
  }
}

}  // namespace

}  // namespace testing
}  // namespace gvisor
