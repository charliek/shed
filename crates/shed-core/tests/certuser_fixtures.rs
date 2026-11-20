//! The certificate user reader (`shed_core::certuser`) against the two-language fixture
//! set `crates/fixtures/clientuser/`, which a Go test in `internal/servertls` writes and
//! checks (see that folder's README.md):
//!
//! - every fixture reads as `expected.json`'s `rust` answer says;
//! - the refusals that matter are errors, never `Unbound`, named here so a mis-edited
//!   `expected.json` cannot relax them;
//! - no truncation of any fixture, no single-byte change of any fixture and no random input
//!   panics: each returns a `Result`. That proves no panic, nothing about agreement with Go.

use std::path::{Path, PathBuf};

use shed_core::certuser::{cert_user, cert_user_pem, CertUser};
use shed_core::csr::pem_decode;

fn fixture_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/clientuser")
}

struct Fixture {
    name: String,
    pem: String,
    rust: String,
}

fn fixtures() -> Vec<Fixture> {
    let dir = fixture_dir();
    let text = std::fs::read_to_string(dir.join("expected.json")).expect("read expected.json");
    let doc: serde_json::Value = serde_json::from_str(&text).expect("parse expected.json");
    let list = doc["fixtures"].as_array().expect("fixtures array");
    assert!(list.len() >= 40, "only {} fixtures", list.len());
    list.iter()
        .map(|f| {
            let name = f["name"].as_str().expect("name").to_owned();
            let file = f["file"].as_str().expect("file");
            Fixture {
                pem: std::fs::read_to_string(dir.join(file))
                    .unwrap_or_else(|e| panic!("read {file}: {e}")),
                rust: f["rust"].as_str().expect("rust").to_owned(),
                name,
            }
        })
        .collect()
}

fn answer(read: &Result<CertUser, shed_core::certuser::CertUserError>) -> String {
    match read {
        Ok(CertUser::Named(name)) => format!("named:{name}"),
        Ok(CertUser::Implicit) => "implicit".into(),
        Ok(CertUser::Unbound) => "unbound".into(),
        Err(_) => "error".into(),
    }
}

fn der_of(f: &Fixture) -> Vec<u8> {
    pem_decode("CERTIFICATE", &f.pem).unwrap_or_else(|e| panic!("{}: {e}", f.name))
}

#[test]
fn every_fixture_reads_as_expected() {
    let all = fixtures();
    // Every .pem in the folder is listed: none is skipped by accident.
    let mut on_disk: Vec<String> = std::fs::read_dir(fixture_dir())
        .expect("read fixture dir")
        .map(|e| e.expect("entry").file_name().to_string_lossy().into_owned())
        .filter(|n| n.ends_with(".pem"))
        .collect();
    on_disk.sort();
    let mut listed: Vec<String> = all.iter().map(|f| format!("{}.pem", f.name)).collect();
    listed.sort();
    assert_eq!(on_disk, listed, "the folder and expected.json disagree");

    for f in &all {
        let got = answer(&cert_user_pem(&f.pem));
        assert_eq!(got, f.rust, "fixture {}", f.name);
        // The DER entry point says the same.
        assert_eq!(
            answer(&cert_user(&der_of(f))),
            f.rust,
            "fixture {} (DER)",
            f.name
        );
    }
}

#[test]
fn refusals_are_errors_never_unbound() {
    let all = fixtures();
    let get = |name: &str| {
        all.iter()
            .find(|f| f.name == name)
            .unwrap_or_else(|| panic!("no fixture {name}"))
    };
    for name in [
        "ia5-pat",
        "t61-pat",
        "bmp-pat",
        "implicit-printable",
        "implicit-ia5",
        "trailing-newline",
        "upper-case",
        "integer-value",
        "two-rdns",
        "two-in-one-rdn",
        "same-user-twice",
        "named-and-implicit",
        "attribute-two-values",
        "nonminimal-length",
        "nonminimal-value-length",
        "indefinite-length",
        "trailing-bytes",
        "fourth-element",
        "high-tag-value",
    ] {
        let read = cert_user_pem(&get(name).pem);
        assert!(read.is_err(), "{name}: read {read:?}, want an error");
    }
    // The one reading of "names no user" is a subject with no userId; one in the issuer
    // or in an extension is never read.
    for name in [
        "issuer-only",
        "extension-only",
        "extension-oid-only",
        "no-user",
        "empty-subject",
        "near-oid",
    ] {
        assert_eq!(
            cert_user_pem(&get(name).pem),
            Ok(CertUser::Unbound),
            "{name}"
        );
    }
    // A user outside the subject never changes the subject's answer.
    for name in ["issuer-and-subject", "subject-and-extension"] {
        assert_eq!(
            cert_user_pem(&get(name).pem),
            Ok(CertUser::Named("pat".into())),
            "{name}"
        );
    }
    assert_eq!(
        cert_user_pem(&get("issued-pat").pem),
        Ok(CertUser::Named("pat".into()))
    );
    assert_eq!(
        cert_user_pem(&get("issued-implicit").pem),
        Ok(CertUser::Implicit)
    );
}

/// A seeded xorshift64*, so a failure is reproducible without a new crate.
struct XorShift(u64);

impl XorShift {
    fn next(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.0 = x;
        x.wrapping_mul(0x2545_F491_4F6C_DD1D)
    }

    fn byte(&mut self) -> u8 {
        (self.next() >> 56) as u8
    }

    fn below(&mut self, n: usize) -> usize {
        (self.next() % n as u64) as usize
    }
}

#[test]
fn every_truncation_returns_a_result() {
    for f in fixtures() {
        let der = der_of(&f);
        for len in 0..der.len() {
            let read = cert_user(&der[..len]);
            // A proper prefix of a certificate the reader accepts is always refused: its
            // outer length runs past the input.
            if f.rust != "error" {
                assert!(
                    read.is_err(),
                    "{}: prefix of {len} bytes read {read:?}",
                    f.name
                );
            }
        }
    }
}

#[test]
fn random_inputs_and_byte_changes_return_a_result() {
    let mut rng = XorShift(0x5EED_C1E7_05E2_2026);
    // 10,000 random byte strings: half raw, half behind a SEQUENCE header whose length is
    // right, so the walk goes past the first check.
    for i in 0..10_000 {
        let len = rng.below(600);
        let body: Vec<u8> = (0..len).map(|_| rng.byte()).collect();
        let input = if i % 2 == 0 {
            body
        } else {
            let mut framed = vec![0x30];
            if len < 0x80 {
                framed.push(len as u8);
            } else if len < 0x100 {
                framed.extend([0x81, len as u8]);
            } else {
                framed.extend([0x82, (len >> 8) as u8, len as u8]);
            }
            framed.extend(body);
            framed
        };
        let _ = cert_user(&input);
    }
    // Every byte of every fixture changed once, to a random other value, and 200 more
    // random single-byte changes per fixture.
    for f in fixtures() {
        let der = der_of(&f);
        for pos in 0..der.len() {
            let mut changed = der.clone();
            changed[pos] ^= rng.byte() | 1;
            let _ = cert_user(&changed);
        }
        for _ in 0..200 {
            let mut changed = der.clone();
            let pos = rng.below(der.len());
            changed[pos] = rng.byte();
            let _ = cert_user(&changed);
        }
    }
}
