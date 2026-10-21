"""Host-side fronthaul peer: counts eCPRI frames from the sandbox on IFACE and
answers with N eCPRI frames once the first one arrives. The host stack moves
802.1Q tags into packet metadata, so tagged frames are counted as plain eCPRI
here; check tags on the wire with tcpdump -e."""
import socket, struct, sys, time
iface, n = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003))
s.bind((iface, 0))
s.settimeout(20)
mac = s.getsockname()[4]
got = vlan = 0
dst = None
deadline = time.time() + 20
while time.time() < deadline and got < n:
    try:
        f = s.recv(2048)
    except socket.timeout:
        break
    et = struct.unpack("!H", f[12:14])[0]
    if et == 0x8100 and struct.unpack("!H", f[16:18])[0] == 0xAEFE:
        got += 1; vlan += 1; dst = f[6:12]
    elif et == 0xAEFE:
        got += 1; dst = f[6:12]
print(f"peer: received {got} eCPRI frames from the sandbox", flush=True)
if dst:
    for i in range(n):
        s.send(dst + mac + b"\xae\xfe\x10\x00\x00\x00" + bytes([i & 0xff]) + b"\0" * 41)
    print(f"peer: sent {n} eCPRI frames", flush=True)
