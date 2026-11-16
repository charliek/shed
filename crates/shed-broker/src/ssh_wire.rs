//! A minimal SSH-wire reader and writer over byte slices (RFC 4251 §5: `byte`,
//! `uint32`, `string`), shared by the agent-forward backend
//! ([`crate::ssh_backend_agent`]) and the sign-request check ([`crate::sign_check`]).
//!
//! Hand-rolled on purpose (no new dependency): every read is bounds-checked against
//! the slice, a declared length is never trusted before the bytes are there (so a
//! `u32::MAX` length allocates nothing and fails cleanly), and nothing here panics on
//! any input. A failed read reports [`Truncated`]; callers map it to their own error.

/// A read ran past the end of the buffer (a short buffer, or a declared `string`
/// length longer than what remains).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Truncated;

/// A cursor over an SSH-wire byte slice. Each successful read advances it; a failed
/// read leaves it where it was.
pub(crate) struct Reader<'a> {
    buf: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    pub(crate) fn new(buf: &'a [u8]) -> Reader<'a> {
        Reader { buf, pos: 0 }
    }

    pub(crate) fn u8(&mut self) -> Result<u8, Truncated> {
        let b = *self.buf.get(self.pos).ok_or(Truncated)?;
        self.pos += 1;
        Ok(b)
    }

    pub(crate) fn u32(&mut self) -> Result<u32, Truncated> {
        let end = self.pos.checked_add(4).ok_or(Truncated)?;
        let slice = self.buf.get(self.pos..end).ok_or(Truncated)?;
        self.pos = end;
        let mut word = [0u8; 4];
        word.copy_from_slice(slice);
        Ok(u32::from_be_bytes(word))
    }

    /// A `uint64`. All eight bytes are checked before any is consumed, so a short
    /// read consumes nothing.
    pub(crate) fn u64(&mut self) -> Result<u64, Truncated> {
        let end = self.pos.checked_add(8).ok_or(Truncated)?;
        let slice = self.buf.get(self.pos..end).ok_or(Truncated)?;
        self.pos = end;
        let mut word = [0u8; 8];
        word.copy_from_slice(slice);
        Ok(u64::from_be_bytes(word))
    }

    /// An SSH `string`: `uint32` length, then that many bytes, borrowed from the
    /// buffer. A length past the remainder is [`Truncated`] and consumes nothing.
    pub(crate) fn string(&mut self) -> Result<&'a [u8], Truncated> {
        let start = self.pos;
        let len = self.u32()? as usize;
        let Some(slice) = self
            .pos
            .checked_add(len)
            .and_then(|end| self.buf.get(self.pos..end))
        else {
            self.pos = start;
            return Err(Truncated);
        };
        self.pos += len;
        Ok(slice)
    }

    /// True when every byte has been consumed (x/crypto's `ssh.Unmarshal` and
    /// OpenSSH's `sshbuf_len(b) != 0` checks both refuse top-level trailing data;
    /// callers use this to do the same).
    pub(crate) fn exhausted(&self) -> bool {
        self.pos == self.buf.len()
    }
}

/// Append an SSH `string` (RFC 4251: `uint32-BE len ‖ bytes`) to `out`. The callers
/// write agent requests and test vectors, far below `u32::MAX`.
pub(crate) fn put_string(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u32).to_be_bytes());
    out.extend_from_slice(bytes);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_each_field_type() {
        let mut buf = vec![7u8];
        buf.extend_from_slice(&0xdead_beef_u32.to_be_bytes());
        buf.extend_from_slice(&u64::MAX.to_be_bytes());
        put_string(&mut buf, b"abc");
        put_string(&mut buf, b"");
        let mut r = Reader::new(&buf);
        assert_eq!(r.u8(), Ok(7));
        assert_eq!(r.u32(), Ok(0xdead_beef));
        assert_eq!(r.u64(), Ok(u64::MAX));
        assert_eq!(r.string(), Ok(&b"abc"[..]));
        assert_eq!(r.string(), Ok(&b""[..]));
        assert!(r.exhausted());
        assert_eq!(r.u8(), Err(Truncated));
        assert_eq!(r.u32(), Err(Truncated));
        assert_eq!(r.string(), Err(Truncated));
    }

    #[test]
    fn declared_length_past_the_remainder_is_truncated_and_consumes_nothing() {
        for declared in [4u32, 5, 1 << 20, u32::MAX] {
            let mut buf = declared.to_be_bytes().to_vec();
            buf.extend_from_slice(b"abc");
            let mut r = Reader::new(&buf);
            assert_eq!(r.string(), Err(Truncated), "declared {declared}");
            // Nothing consumed: the next read starts at the length word again.
            assert_eq!(r.u32(), Ok(declared));
        }
    }

    #[test]
    fn a_short_u64_is_truncated_and_consumes_nothing() {
        // Every length short of eight, including a whole first half (the case that
        // used to consume four bytes before failing).
        for n in 0..8 {
            let buf: Vec<u8> = (1..=n as u8).collect();
            let mut r = Reader::new(&buf);
            assert_eq!(r.u64(), Err(Truncated), "{n} bytes");
            assert_eq!(r.pos, 0, "{n} bytes: nothing consumed");
            for &b in &buf {
                assert_eq!(r.u8(), Ok(b));
            }
            assert!(r.exhausted());
        }
    }

    #[test]
    fn short_u32_is_truncated() {
        for n in 0..4 {
            let buf = vec![0u8; n];
            assert_eq!(Reader::new(&buf).u32(), Err(Truncated));
            assert_eq!(Reader::new(&buf).string(), Err(Truncated));
        }
    }
}
