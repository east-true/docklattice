mod server_client;

use serde_json::Value;
use tauri::{
    image::Image,
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    AppHandle, Manager, State, Wry,
};

use server_client::{Connection, ServerClient};

const TRAY_ID: &str = "compose-widget-tray";

struct TrayState {
    status_menu: MenuItem<Wry>,
    base_icon: Image<'static>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TrayStatus {
    Disconnected,
    Connecting,
    Connected,
    Error,
}

impl TrayStatus {
    fn parse(value: &str) -> Option<Self> {
        match value {
            "disconnected" => Some(Self::Disconnected),
            "connecting" => Some(Self::Connecting),
            "connected" => Some(Self::Connected),
            "error" => Some(Self::Error),
            _ => None,
        }
    }

    fn label(self) -> &'static str {
        match self {
            Self::Disconnected => "Not connected",
            Self::Connecting => "Connecting",
            Self::Connected => "Connected",
            Self::Error => "Connection unavailable",
        }
    }

    fn color(self) -> [u8; 4] {
        match self {
            Self::Disconnected => [128, 139, 153, 255],
            Self::Connecting => [217, 150, 32, 255],
            Self::Connected => [27, 143, 88, 255],
            Self::Error => [202, 61, 55, 255],
        }
    }
}

fn tray_status_icon(base_icon: &Image<'_>, status: TrayStatus) -> Image<'static> {
    let width = base_icon.width();
    let height = base_icon.height();
    let reference = width.min(height) as i32;
    let outer_radius = (reference * 3 / 16).max(3);
    let border = (reference / 16).max(1);
    let inner_radius = outer_radius - border;
    let margin = (reference / 32).max(1);
    let center_x = width as i32 - outer_radius - margin;
    let center_y = outer_radius + margin;
    let mut rgba = base_icon.rgba().to_vec();

    for y in (center_y - outer_radius)..=(center_y + outer_radius) {
        for x in (center_x - outer_radius)..=(center_x + outer_radius) {
            if x < 0 || y < 0 || x >= width as i32 || y >= height as i32 {
                continue;
            }

            let distance = (x - center_x).pow(2) + (y - center_y).pow(2);
            let pixel = if distance <= inner_radius.pow(2) {
                Some(status.color())
            } else if distance <= outer_radius.pow(2) {
                Some([255, 255, 255, 255])
            } else {
                None
            };

            if let Some(pixel) = pixel {
                let offset = ((y as u32 * width + x as u32) * 4) as usize;
                rgba[offset..offset + 4].copy_from_slice(&pixel);
            }
        }
    }

    Image::new_owned(rgba, width, height)
}

#[tauri::command]
fn set_tray_status(
    app: AppHandle,
    tray_state: State<'_, TrayState>,
    status: String,
) -> Result<(), String> {
    let status = TrayStatus::parse(&status).ok_or_else(|| "Unknown tray status".to_string())?;
    let tray = app
        .tray_by_id(TRAY_ID)
        .ok_or_else(|| "Tray icon is unavailable".to_string())?;

    tray.set_icon(Some(tray_status_icon(&tray_state.base_icon, status)))
        .map_err(|error| format!("Could not update the tray icon: {error}"))?;
    tray.set_tooltip(Some(format!("Compose widget — {}", status.label())))
        .map_err(|error| format!("Could not update the tray tooltip: {error}"))?;
    tray_state
        .status_menu
        .set_text(format!("Status: {}", status.label()))
        .map_err(|error| format!("Could not update the tray menu: {error}"))?;

    Ok(())
}

#[tauri::command]
async fn dashboard(connection: Connection) -> Result<Value, String> {
    ServerClient::new(connection)?.get_dashboard().await
}

#[tauri::command]
async fn project_runtime(connection: Connection, project_uid: String) -> Result<Value, String> {
    ServerClient::new(connection)?
        .get_project_runtime(&project_uid)
        .await
}

#[tauri::command]
async fn start_operation(
    connection: Connection,
    agent_id: String,
    project_uid: String,
    kind: String,
    target: Option<String>,
) -> Result<Value, String> {
    ServerClient::new(connection)?
        .start_operation(&agent_id, &project_uid, &kind, target.as_deref())
        .await
}

#[tauri::command]
async fn operation(
    connection: Connection,
    agent_id: String,
    operation_id: String,
) -> Result<Value, String> {
    ServerClient::new(connection)?
        .get_operation(&agent_id, &operation_id)
        .await
}

fn show_main_window(app: &tauri::AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
}

fn toggle_main_window(app: &tauri::AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        match window.is_visible() {
            Ok(true) => {
                let _ = window.hide();
            }
            _ => show_main_window(app),
        }
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let initial_status = TrayStatus::Disconnected;
            let configured_icon = app
                .default_window_icon()
                .ok_or("The application icon is unavailable")?;
            let base_icon = Image::new_owned(
                configured_icon.rgba().to_vec(),
                configured_icon.width(),
                configured_icon.height(),
            );
            let status = MenuItem::with_id(
                app,
                "status",
                format!("Status: {}", initial_status.label()),
                false,
                None::<&str>,
            )?;
            let open = MenuItem::with_id(app, "open", "Open widget", true, None::<&str>)?;
            let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&status, &open, &quit])?;

            TrayIconBuilder::with_id(TRAY_ID)
                .menu(&menu)
                .show_menu_on_left_click(false)
                .tooltip(format!("Compose widget — {}", initial_status.label()))
                .icon(tray_status_icon(&base_icon, initial_status))
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "open" => show_main_window(app),
                    "quit" => app.exit(0),
                    _ => {}
                })
                .on_tray_icon_event(|tray, event| {
                    if let TrayIconEvent::Click {
                        button: MouseButton::Left,
                        button_state: MouseButtonState::Up,
                        ..
                    } = event
                    {
                        toggle_main_window(tray.app_handle());
                    }
                })
                .build(app)?;
            app.manage(TrayState {
                status_menu: status,
                base_icon,
            });
            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .invoke_handler(tauri::generate_handler![
            dashboard,
            project_runtime,
            start_operation,
            operation,
            set_tray_status
        ])
        .run(tauri::generate_context!())
        .expect("failed to run the desktop widget");
}

#[cfg(test)]
mod tests {
    use super::{tray_status_icon, TrayStatus};
    use tauri::image::Image;

    #[test]
    fn tray_statuses_have_stable_labels_and_colors() {
        assert_eq!(TrayStatus::parse("connected"), Some(TrayStatus::Connected));
        assert_eq!(TrayStatus::Connected.label(), "Connected");
        assert_eq!(TrayStatus::Error.color(), [202, 61, 55, 255]);
        assert_eq!(TrayStatus::parse("unknown"), None);
    }

    #[test]
    fn tray_status_icon_preserves_the_base_and_adds_a_top_right_badge() {
        let base = Image::new_owned(vec![10; 32 * 32 * 4], 32, 32);
        let icon = tray_status_icon(&base, TrayStatus::Connecting);

        assert_eq!(icon.width(), 32);
        assert_eq!(icon.height(), 32);
        assert_eq!(icon.rgba().len(), 32 * 32 * 4);
        assert_eq!(&icon.rgba()[0..4], &[10, 10, 10, 10]);

        let badge_center = ((7 * 32 + 25) * 4) as usize;
        assert_eq!(
            &icon.rgba()[badge_center..badge_center + 4],
            &TrayStatus::Connecting.color()
        );
    }
}
