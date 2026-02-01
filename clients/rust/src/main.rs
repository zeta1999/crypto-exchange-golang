use serde::Serialize;
use tungstenite::{connect, Message};
use url::Url;

#[derive(Serialize)]
struct Command<'a> {
    command: &'a str,
    instrument: &'a str,
    token: &'a str,
}

fn main() {
    let (mut socket, _response) = connect(Url::parse("ws://localhost:8081/ws").unwrap()).expect("websocket connect");
    let cmd = Command {
        command: "snapshot",
        instrument: "BTC-USD",
        token: "dev-secret-token",
    };
    let payload = serde_json::to_string(&cmd).unwrap();
    socket.write_message(Message::Text(payload)).expect("send command");
    if let Ok(msg) = socket.read_message() {
        println!("snapshot => {}", msg);
    }
}
