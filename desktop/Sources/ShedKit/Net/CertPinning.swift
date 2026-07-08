// CertPinning.swift
//
// TLS certificate pinning for shed-server's self-signed HTTPS cert. The pin is
// the SHA-256 of the leaf cert's DER as "sha256:<lowercase-hex>", matching the
// server (internal/servertls.Fingerprint) and the Go sdk + CLI clients.

import CryptoKit
import Foundation

/// Returns the pin string "sha256:<lowercase-hex>" for a DER-encoded cert,
/// byte-for-byte compatible with the server and Go clients.
public func certFingerprint(_ der: Data) -> String {
    let digest = SHA256.hash(data: der)
    return "sha256:" + digest.map { String(format: "%02x", $0) }.joined()
}

/// Pure pin decision: does the leaf cert's DER hash to the expected pin?
public func pinMatches(leafDER: Data, fingerprint: String) -> Bool {
    return certFingerprint(leafDER) == fingerprint
}

/// URLSession delegate that pins the server's self-signed cert by fingerprint.
/// Fail-closed: any server-trust challenge it cannot verify against the pin is
/// cancelled, so a mismatched (or unreadable) cert never completes a handshake.
public final class PinningSessionDelegate: NSObject, URLSessionDelegate, URLSessionTaskDelegate, @unchecked Sendable {
    // @unchecked Sendable: the only stored state is an immutable pin string.
    private let fingerprint: String

    public init(fingerprint: String) {
        self.fingerprint = fingerprint
    }

    /// A pinned session must never follow a redirect to plaintext. Allow only
    /// https redirects (their handshake is re-pinned by the challenge handler
    /// below, so an off-pin host is rejected there); cancel anything else.
    public func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        if request.url?.scheme?.lowercased() == "https" {
            completionHandler(request)
        } else {
            completionHandler(nil)  // don't follow; the task returns the redirect response
        }
    }

    public func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust,
              let leaf = pinningLeafCertificate(trust)
        else {
            completionHandler(.cancelAuthenticationChallenge, nil)
            return
        }
        let der = SecCertificateCopyData(leaf) as Data
        if pinMatches(leafDER: der, fingerprint: fingerprint) {
            completionHandler(.useCredential, URLCredential(trust: trust))
        } else {
            completionHandler(.cancelAuthenticationChallenge, nil)
        }
    }
}

/// The leaf (index 0) certificate of a SecTrust, across OS versions.
func pinningLeafCertificate(_ trust: SecTrust) -> SecCertificate? {
    if #available(macOS 12.0, iOS 15.0, *) {
        return (SecTrustCopyCertificateChain(trust) as? [SecCertificate])?.first
    }
    return SecTrustGetCertificateAtIndex(trust, 0)
}
