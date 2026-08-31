use std::time::Duration;

use reqwest::{redirect::Policy, Certificate, Client, Method, Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use uuid::Uuid;

const MAX_CA_PEM_BYTES: usize = 64 * 1024;
const MAX_RESPONSE_BYTES: usize = 8 * 1024 * 1024;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const ALLOWED_OPERATIONS: [&str; 5] = [
    "compose.pull",
    "compose.up",
    "compose.down",
    "compose.start",
    "compose.stop",
];

#[derive(Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Connection {
    server_url: String,
    ca_pem: Option<String>,
}

pub struct ServerClient {
    base_url: Url,
    client: Client,
}

#[derive(Serialize)]
struct OperationRequest<'a> {
    operation_id: String,
    agent_id: &'a str,
    project_uid: &'a str,
    kind: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    target: Option<&'a str>,
}

impl ServerClient {
    pub fn new(connection: Connection) -> Result<Self, String> {
        let base_url = validate_server_url(&connection.server_url)?;
        let mut builder = Client::builder()
            .redirect(Policy::none())
            .no_proxy()
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT);

        if let Some(ca_pem) = connection.ca_pem.filter(|pem| !pem.trim().is_empty()) {
            if ca_pem.len() > MAX_CA_PEM_BYTES {
                return Err("The Server CA certificate is too large.".into());
            }
            let certificates = Certificate::from_pem_bundle(ca_pem.as_bytes())
                .map_err(|_| "The Server CA certificate is not valid PEM.".to_string())?;
            if certificates.is_empty() {
                return Err("The Server CA certificate contains no certificates.".into());
            }
            for certificate in certificates {
                builder = builder.add_root_certificate(certificate);
            }
        }

        let client = builder
            .build()
            .map_err(|error| format!("Could not configure the Server connection: {error}"))?;
        Ok(Self { base_url, client })
    }

    pub async fn get_dashboard(&self) -> Result<Value, String> {
        let url = self.endpoint(&["api", "v1", "dashboard"])?;
        self.request_json(Method::GET, url, None).await
    }

    pub async fn get_project_runtime(&self, project_uid: &str) -> Result<Value, String> {
        validate_identifier("Project", project_uid)?;
        let url = self.endpoint(&["api", "v1", "projects", project_uid, "runtime"])?;
        self.request_json(Method::GET, url, None).await
    }

    pub async fn start_operation(
        &self,
        agent_id: &str,
        project_uid: &str,
        kind: &str,
        target: Option<&str>,
    ) -> Result<Value, String> {
        validate_identifier("Agent", agent_id)?;
        validate_identifier("Project", project_uid)?;
        let target = validate_operation(kind, target)?;

        let request = OperationRequest {
            operation_id: format!("widget-{}", Uuid::new_v4()),
            agent_id,
            project_uid,
            kind,
            target,
        };
        let body = serde_json::to_value(request)
            .map_err(|error| format!("Could not encode the operation: {error}"))?;
        let url = self.endpoint(&["api", "v1", "operations"])?;
        self.request_json(Method::POST, url, Some(body)).await
    }

    pub async fn get_operation(&self, agent_id: &str, operation_id: &str) -> Result<Value, String> {
        validate_identifier("Agent", agent_id)?;
        validate_identifier("Operation", operation_id)?;
        let url = self.endpoint(&["api", "v1", "agents", agent_id, "operations", operation_id])?;
        self.request_json(Method::GET, url, None).await
    }

    fn endpoint(&self, segments: &[&str]) -> Result<Url, String> {
        let mut url = self.base_url.clone();
        let mut path = url
            .path_segments_mut()
            .map_err(|_| "The Server URL cannot be used as a base URL.".to_string())?;
        path.clear();
        for segment in segments {
            path.push(segment);
        }
        drop(path);
        Ok(url)
    }

    async fn request_json(
        &self,
        method: Method,
        url: Url,
        body: Option<Value>,
    ) -> Result<Value, String> {
        let mut request = self.client.request(method, url);
        if let Some(body) = body {
            request = request.json(&body);
        }
        let mut response = request
            .send()
            .await
            .map_err(|error| format!("Could not reach the Server: {error}"))?;
        let status = response.status();

        if response
            .content_length()
            .is_some_and(|length| length > MAX_RESPONSE_BYTES as u64)
        {
            return Err("The Server response exceeds the widget safety limit.".into());
        }

        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|error| format!("Could not read the Server response: {error}"))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err("The Server response exceeds the widget safety limit.".into());
            }
            bytes.extend_from_slice(&chunk);
        }

        if !status.is_success() {
            let detail = serde_json::from_slice::<Value>(&bytes)
                .ok()
                .and_then(|value| {
                    let message = value
                        .get("message")
                        .or_else(|| value.get("error"))
                        .and_then(Value::as_str)
                        .map(str::to_string)?;
                    let code = value.get("code").and_then(Value::as_str);
                    Some(match code {
                        Some(code) => format!("{code}: {message}"),
                        None => message,
                    })
                })
                .unwrap_or_else(|| status.canonical_reason().unwrap_or("request failed").into());
            return Err(format!("Server request failed ({status}): {detail}"));
        }

        serde_json::from_slice(&bytes)
            .map_err(|_| "The Server returned an invalid JSON response.".to_string())
    }
}

fn validate_server_url(value: &str) -> Result<Url, String> {
    let mut url = Url::parse(value.trim()).map_err(|_| "Enter a valid Server URL.".to_string())?;
    if url.scheme() != "https" {
        return Err("This widget requires an HTTPS Server URL.".into());
    }
    if url.host_str().is_none() {
        return Err("The Server URL must include a host.".into());
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("Credentials are not allowed in the Server URL.".into());
    }
    if url.query().is_some() || url.fragment().is_some() {
        return Err("The Server URL cannot include a query or fragment.".into());
    }
    if !matches!(url.path(), "" | "/") {
        return Err("The Server URL cannot include a path.".into());
    }
    url.set_path("/");
    Ok(url)
}

fn validate_identifier(label: &str, value: &str) -> Result<(), String> {
    if value.is_empty() || value.len() > 512 || value.chars().any(char::is_control) {
        return Err(format!("The {label} identifier is invalid."));
    }
    Ok(())
}

fn validate_operation<'a>(kind: &str, target: Option<&'a str>) -> Result<Option<&'a str>, String> {
    if !ALLOWED_OPERATIONS.contains(&kind) {
        return Err("This operation is not available in this widget.".into());
    }

    let target = target.filter(|value| !value.is_empty());
    if matches!(kind, "compose.pull" | "compose.up" | "compose.down") && target.is_some() {
        return Err("This project operation cannot have a Service target.".into());
    }
    if let Some(value) = target {
        validate_identifier("Service", value)?;
    }
    Ok(target)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn connection(url: &str) -> Connection {
        Connection {
            server_url: url.into(),
            ca_pem: None,
        }
    }

    #[test]
    fn server_url_requires_a_clean_https_origin() {
        assert!(ServerClient::new(connection("https://127.0.0.1:8080")).is_ok());
        assert!(ServerClient::new(connection("http://127.0.0.1:8080")).is_err());
        assert!(ServerClient::new(connection("https://user@example.test")).is_err());
        assert!(ServerClient::new(connection("https://example.test/base")).is_err());
        assert!(ServerClient::new(connection("https://example.test/?token=x")).is_err());
    }

    #[test]
    fn endpoints_encode_each_identifier_as_one_path_segment() {
        let client = ServerClient::new(connection("https://example.test:8443")).unwrap();
        let endpoint = client
            .endpoint(&["api", "v1", "projects", "project/../../other", "runtime"])
            .unwrap();
        assert_eq!(
            endpoint.as_str(),
            "https://example.test:8443/api/v1/projects/project%2F..%2F..%2Fother/runtime"
        );
    }

    #[test]
    fn identifiers_reject_empty_oversized_and_control_characters() {
        assert!(validate_identifier("Project", "project-a").is_ok());
        assert!(validate_identifier("Project", "").is_err());
        assert!(validate_identifier("Project", &"a".repeat(513)).is_err());
        assert!(validate_identifier("Project", "project\nother").is_err());
    }

    #[test]
    fn operations_are_limited_to_the_widget_contract() {
        for kind in ALLOWED_OPERATIONS {
            assert!(validate_operation(kind, None).is_ok(), "{kind}");
        }
        assert!(validate_operation("compose.restart", None).is_err());
        assert_eq!(
            validate_operation("compose.start", Some("api")).unwrap(),
            Some("api")
        );
        assert!(validate_operation("compose.pull", Some("api")).is_err());
        assert!(validate_operation("compose.up", Some("api")).is_err());
        assert!(validate_operation("compose.down", Some("api")).is_err());
        assert!(validate_operation("compose.stop", Some("api\nworker")).is_err());
    }
}
