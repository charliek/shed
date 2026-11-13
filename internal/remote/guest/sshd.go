package guest

// SSHDConfig is the content of SSHDConfigFile in a layout-1 image: the whole
// of the guest sshd's policy. The image build writes it verbatim, and the
// transport's real-sshd tests run OpenSSH with exactly this text, so what is
// tested is what is baked. Changing it is a new guest layout.
//
// The three forwarding lines work together, and their form was settled by
// test against OpenSSH 9.6 (Ubuntu 24.04), not by the manual:
//
//   - "AllowTcpForwarding no" also refuses forwarded unix sockets, whatever
//     AllowStreamLocalForwarding says, because sshd keeps one permission
//     list for both. So TCP forwarding has to be "local".
//   - "local" alone would let the key holder open TCP connections from the
//     guest to anywhere. PermitOpen takes that back: with a PermitOpen list,
//     sshd forwards only to the listed targets, and a bracketed socket path
//     with port "*" is how a unix socket is listed. The list is the agent's
//     three sockets, so those are the only things the server's key can
//     reach, and no TCP target matches.
//   - AllowStreamLocalForwarding local (not "yes") refuses the other
//     direction: the guest never listens on the server's behalf.
const SSHDConfig = `PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowUsers shed-link
HostKey /etc/ssh/ssh_host_ed25519_key
AuthorizedKeysFile /etc/ssh/shed-link/authorized_keys
AllowStreamLocalForwarding local
AllowTcpForwarding local
PermitOpen [/run/shed-agent/1024.sock]:* [/run/shed-agent/1026.sock]:* [/run/shed-agent/1028.sock]:*
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
PermitTTY no
PermitUserRC no
`
