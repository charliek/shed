//! Test mode's mock URL, per server.
//!
//! In test mode every configured server is redirected to a hermetic mock. The mock URL
//! (`SHED_TAURI_MOCK_BASE_URL`) is either one URL, which every server is sent to, or a
//! comma list whose first element is that default and whose others are
//! `<server>=<url>` overrides: `http://127.0.0.1:4001,mock2=http://127.0.0.1:4002`
//! sends `mock2` to the second mock and every other server to the first. This lets a
//! harness drive two servers that answer differently.

/// The URL test mode sends `server` to under the mock URL `mock`.
pub fn mock_url_for(mock: &str, server: &str) -> String {
    let mut parts = mock.split(',').map(str::trim);
    let default = parts.next().unwrap_or("");
    for part in parts {
        if let Some((name, url)) = part.split_once('=') {
            if name.trim() == server {
                return url.trim().to_string();
            }
        }
    }
    default.to_string()
}

#[cfg(test)]
mod tests {
    use super::mock_url_for;

    #[test]
    fn one_url_is_every_servers() {
        assert_eq!(
            mock_url_for("http://127.0.0.1:9", "a"),
            "http://127.0.0.1:9"
        );
        assert_eq!(
            mock_url_for("http://127.0.0.1:9", "b"),
            "http://127.0.0.1:9"
        );
    }

    #[test]
    fn an_override_names_its_server_and_the_rest_take_the_default() {
        let mock = "http://127.0.0.1:1000, b=http://127.0.0.1:2000,c=http://127.0.0.1:3000";
        assert_eq!(mock_url_for(mock, "a"), "http://127.0.0.1:1000");
        assert_eq!(mock_url_for(mock, "b"), "http://127.0.0.1:2000");
        assert_eq!(mock_url_for(mock, "c"), "http://127.0.0.1:3000");
        assert_eq!(mock_url_for(mock, "b=x"), "http://127.0.0.1:1000");
    }
}
