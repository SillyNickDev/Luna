//! Write a Luna plugin in Rust.
//!
//! ```no_run
//! let mut bot = luna::Bot::new();
//! bot.on("twitch.channel.follow", |ctx, ev| async move {
//!     ctx.chat().send(format!("Welcome to the pack, {}!", ev.user().map(|u| u.name).unwrap_or_default())).await
//! });
//! bot.run();
//! ```
//!
//! A Rust plugin is a `native` capsule: build it as a static binary
//! (`RUSTFLAGS="-C target-feature=+crt-static"`) and put it at `bin/linux-amd64/<entry>` in the package.
//! shortcut: no in-process test harness or bot.http yet; test against luna capsule dev, add them when Rust plugins need them.

mod frames;
mod pb;

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Map, Value, json};
use tokio::sync::{Mutex, mpsc};
use tonic::codegen::http::uri::PathAndQuery;
use tonic::transport::{Channel, Endpoint};

pub use serde_json;

const API: &str = "1";
const SDK: &str = "luna-rust/0.1.0";

/// Errors from Luna, matched by kind; the message says how to fix it (e.g. the manifest line to add).
#[derive(Debug, Clone)]
pub enum Error {
    PermissionDenied(String),
    RateLimited(String),
    NotConnected(String),
    Timeout(String),
    LinkUnavailable(String),
    Other(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::PermissionDenied(m) => write!(f, "permission denied: {m}"),
            Error::RateLimited(m) => write!(f, "rate limited: {m}"),
            Error::NotConnected(m) => write!(f, "not connected: {m}"),
            Error::Timeout(m) => write!(f, "timed out: {m}"),
            Error::LinkUnavailable(m) => write!(f, "link unavailable: {m}"),
            Error::Other(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<tonic::Status> for Error {
    fn from(s: tonic::Status) -> Self {
        let m = s.message().to_string();
        match s.code() {
            tonic::Code::PermissionDenied => Error::PermissionDenied(m),
            tonic::Code::ResourceExhausted => Error::RateLimited(m),
            tonic::Code::Unavailable => Error::NotConnected(m),
            tonic::Code::DeadlineExceeded => Error::Timeout(m),
            _ => Error::Other(m),
        }
    }
}

impl From<serde_json::Error> for Error {
    fn from(e: serde_json::Error) -> Self {
        Error::Other(e.to_string())
    }
}

pub type Result<T> = std::result::Result<T, Error>;
type BoxFuture<T> = Pin<Box<dyn Future<Output = T> + Send>>;

/// Someone in chat or an event: rank is set on chat messages.
#[derive(Debug, Clone, Default, serde::Deserialize)]
pub struct User {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub login: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub roles: Vec<String>,
    #[serde(default)]
    pub rank: String,
}

impl User {
    const RANKS: [&str; 5] = ["viewer", "moderator", "luna-mod", "head-mod", "broadcaster"];

    /// Whether the user's rank is at least `rank` ("moderator", "luna-mod", ...).
    pub fn rank_at_least(&self, rank: &str) -> bool {
        let pos = |r: &str| Self::RANKS.iter().position(|x| *x == r);
        pos(if self.rank.is_empty() { "viewer" } else { &self.rank }) >= pos(rank)
    }
}

/// An event: its type, where it came from, and its payload.
#[derive(Debug, Clone)]
pub struct Event {
    pub id: String,
    pub r#type: String,
    pub source: String,
    pub ts_ms: i64,
    pub simulated: bool,
    pub data: Value,
}

impl Event {
    fn from_pb(e: pb::Event) -> Self {
        Event {
            id: e.id,
            r#type: e.r#type,
            source: e.source,
            ts_ms: e.ts_ms,
            simulated: e.simulated,
            data: serde_json::from_slice(&e.data_json).unwrap_or(Value::Null),
        }
    }

    pub fn user(&self) -> Option<User> {
        serde_json::from_value(self.data.get("user")?.clone()).ok()
    }

    pub fn str(&self, k: &str) -> String {
        self.data.get(k).and_then(Value::as_str).unwrap_or_default().to_string()
    }

    pub fn int(&self, k: &str) -> i64 {
        self.data.get(k).and_then(Value::as_i64).unwrap_or_default()
    }
}

/// A chat command, checked by Luna (capability, cooldowns) before the plugin sees it.
#[derive(Debug, Clone, Default, Serialize)]
pub struct ChatCommand {
    pub name: String,
    pub aliases: Vec<String>,
    pub capability: Option<String>,
    pub cooldown: Option<String>,
    pub user_cooldown: Option<String>,
}

impl ChatCommand {
    pub fn new(name: &str) -> Self {
        ChatCommand { name: name.into(), ..Default::default() }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct Arg {
    pub name: String,
    pub r#type: String, // str, int, float or bool
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub optional: bool,
}

/// `luna <plugin> <name>`: its handler gets the parsed args and flags.
#[derive(Debug, Clone, Default, Serialize)]
pub struct CliCommand {
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<Arg>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub flags: Vec<Arg>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub mutates: bool,
}

impl CliCommand {
    pub fn new(name: &str, summary: &str) -> Self {
        CliCommand { name: name.into(), summary: summary.into(), ..Default::default() }
    }
}

type EventFn = Arc<dyn Fn(Ctx, Event) -> BoxFuture<Result<()>> + Send + Sync>;
type ChatFn = Arc<dyn Fn(Ctx, Vec<String>) -> BoxFuture<Result<()>> + Send + Sync>;
type CliFn = Arc<dyn Fn(Ctx, Map<String, Value>) -> BoxFuture<Result<Value>> + Send + Sync>;
type ProvideFn = Arc<dyn Fn(Ctx, String, Value) -> BoxFuture<Result<Value>> + Send + Sync>;
type TaskFn = Arc<dyn Fn(Ctx) -> BoxFuture<Result<()>> + Send + Sync>;

/// The plugin: declare handlers, commands and links, then [`Bot::run`].
#[derive(Default)]
pub struct Bot {
    handlers: Vec<(String, EventFn)>,
    chat: Vec<(ChatCommand, ChatFn)>,
    cli: Vec<(CliCommand, CliFn)>,
    provides: Vec<(String, ProvideFn)>,
    uses: Vec<String>,
    config: Map<String, Value>,
    on_start: Vec<TaskFn>,
    every: Vec<(Duration, TaskFn)>,
}

impl Bot {
    pub fn new() -> Self {
        Self::default()
    }

    /// Handle events matching `pattern` ("twitch.channel.follow" or "twitch.*"), in order.
    pub fn on<F, Fut>(&mut self, pattern: &str, f: F)
    where
        F: Fn(Ctx, Event) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<()>> + Send + 'static,
    {
        self.handlers.push((pattern.into(), Arc::new(move |c, e| Box::pin(f(c, e)))));
    }

    /// `!name`: the handler gets the words after it.
    pub fn chat_command<F, Fut>(&mut self, cmd: ChatCommand, f: F)
    where
        F: Fn(Ctx, Vec<String>) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<()>> + Send + 'static,
    {
        self.chat.push((cmd, Arc::new(move |c, a| Box::pin(f(c, a)))));
    }

    /// `luna <plugin> <name>`: return a string to print, or JSON for a table.
    pub fn cli<F, Fut>(&mut self, cmd: CliCommand, f: F)
    where
        F: Fn(Ctx, Map<String, Value>) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<Value>> + Send + 'static,
    {
        self.cli.push((cmd, Arc::new(move |c, a| Box::pin(f(c, a)))));
    }

    /// Answer calls on link `name` (also in `[links].provides`); the handler gets the calling plugin's ID.
    pub fn provide<F, Fut>(&mut self, name: &str, f: F)
    where
        F: Fn(Ctx, String, Value) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<Value>> + Send + 'static,
    {
        self.provides.push((name.into(), Arc::new(move |c, caller, a| Box::pin(f(c, caller, a)))));
    }

    /// Declare a link this plugin calls, as "provider:name" (also in `[links].uses`).
    pub fn uses(&mut self, link: &str) {
        self.uses.push(link.into());
    }

    /// Settings with their defaults: each field becomes a `[config]` entry (int, float, str,
    /// bool, or a list of tables). Read them with [`Ctx::config`].
    pub fn use_config<T: Serialize + Default>(&mut self) {
        if let Ok(Value::Object(m)) = serde_json::to_value(T::default()) {
            self.config = m;
        }
    }

    pub fn on_start<F, Fut>(&mut self, f: F)
    where
        F: Fn(Ctx) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<()>> + Send + 'static,
    {
        self.on_start.push(Arc::new(move |c| Box::pin(f(c))));
    }

    /// Run `f` every `d`, first after `d`.
    pub fn every<F, Fut>(&mut self, d: Duration, f: F)
    where
        F: Fn(Ctx) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<()>> + Send + 'static,
    {
        self.every.push((d, Arc::new(move |c| Box::pin(f(c)))));
    }

    /// What `luna capsule dev` writes into plugin.toml.
    pub fn describe(&self) -> Value {
        let mut events: Vec<&String> = self.handlers.iter().map(|(p, _)| p).collect();
        events.sort();
        events.dedup();
        let mut cli: Vec<&CliCommand> = self.cli.iter().map(|(c, _)| c).collect();
        cli.sort_by(|a, b| a.name.cmp(&b.name));
        let mut chat: Vec<&ChatCommand> = self.chat.iter().map(|(c, _)| c).collect();
        chat.sort_by(|a, b| a.name.cmp(&b.name));
        let config: Map<String, Value> = self
            .config
            .iter()
            .map(|(k, v)| {
                let t = match v {
                    Value::Bool(_) => "bool",
                    Value::String(_) => "str",
                    Value::Number(n) if n.is_f64() => "float",
                    Value::Number(_) => "int",
                    _ => "list",
                };
                (k.clone(), json!({"type": t, "default": v}))
            })
            .collect();
        let mut provides: Vec<&String> = self.provides.iter().map(|(n, _)| n).collect();
        provides.sort();
        let mut uses = self.uses.clone();
        uses.sort();
        json!({"events": events, "cli": cli, "chat": chat, "config": config,
               "links": {"provides": provides, "uses": uses}})
    }

    /// Connect to Luna and run until it says stop. Under `LUNA_DESCRIBE` it prints the
    /// description instead (that's how `luna capsule dev` reads it).
    pub fn run(self) {
        if std::env::var_os("LUNA_DESCRIBE").is_some() {
            println!("{}", self.describe());
            return;
        }
        if std::env::var_os("CAPSULE_CONTROL").is_none() {
            eprintln!("this is a Luna plugin; run it with: luna capsule dev");
            std::process::exit(1);
        }
        let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
        if let Err(e) = rt.block_on(live(self)) {
            eprintln!("{e}");
            std::process::exit(1); // the supervisor restarts the capsule
        }
    }
}

struct Shared {
    host: tonic::client::Grpc<Channel>,
    id: String,
    dev: bool,
    config: Value,
    links: Mutex<HashMap<String, frames::Conn>>,
}

/// What every handler gets: the API groups, and the event it's answering (replies to a
/// simulated event stay simulated).
#[derive(Clone)]
pub struct Ctx {
    shared: Arc<Shared>,
    event: Option<Event>,
}

impl Ctx {
    pub fn event(&self) -> Option<&Event> {
        self.event.as_ref()
    }
    pub fn user(&self) -> Option<User> {
        self.event.as_ref()?.user()
    }
    /// The plugin's ID, as installed.
    pub fn id(&self) -> &str {
        &self.shared.id
    }
    /// Running under `luna capsule dev`: chat never reaches Twitch.
    pub fn dev(&self) -> bool {
        self.shared.dev
    }
    fn simulated(&self) -> bool {
        self.event.as_ref().is_some_and(|e| e.simulated)
    }

    /// The plugin's settings (see [`Bot::use_config`]).
    pub fn config<T: DeserializeOwned>(&self) -> Result<T> {
        Ok(serde_json::from_value(self.shared.config.clone())?)
    }

    async fn unary<Req, Res>(&self, method: &str, req: Req) -> Result<Res>
    where
        Req: prost::Message + 'static,
        Res: prost::Message + Default + 'static,
    {
        let mut host = self.shared.host.clone();
        host.ready().await.map_err(|e| Error::NotConnected(e.to_string()))?;
        let path = PathAndQuery::try_from(format!("/luna.plugin.v1.Host/{method}")).expect("path");
        let mut r = tonic::Request::new(req);
        r.set_timeout(Duration::from_secs(35));
        Ok(host.unary(r, path, tonic_prost::ProstCodec::default()).await?.into_inner())
    }

    /// Answer in chat, threaded to the message that ran the command.
    pub async fn reply(&self, text: impl Into<String>) -> Result<()> {
        let reply_to = self.event.as_ref().map(|e| e.str("message_id")).unwrap_or_default();
        self.unary::<_, pb::Empty>("ChatSend", pb::ChatSendRequest { text: text.into(), simulated: self.simulated(), reply_to,
            ..Default::default() }).await?;
        Ok(())
    }

    pub fn chat(&self) -> Chat<'_> {
        Chat(self)
    }
    pub fn moderation(&self) -> Mod<'_> {
        Mod(self)
    }
    pub fn store(&self) -> Store<'_> {
        Store(self)
    }
    pub fn twitch(&self) -> Twitch<'_> {
        Twitch(self)
    }
    pub fn osc(&self) -> Osc<'_> {
        Osc(self)
    }

    /// Luna's shared profile of a viewer (by ID or login); None if Luna hasn't seen them. Needs `viewers = true`.
    pub async fn viewer(&self, user: &str) -> Result<Option<Value>> {
        let r: pb::DataReply = self.unary("Viewer", pb::ViewerRequest { user: user.into() }).await?;
        Ok(serde_json::from_slice::<Value>(&r.data_json).ok().filter(|v| !v.is_null()))
    }

    /// The latest heart rate (0 before the first). Needs `hr.update` in `[permissions].events`.
    pub async fn bpm(&self) -> Result<i32> {
        let r: pb::HrReply = self.unary("Hr", pb::HrRequest { minutes: 0 }).await?;
        Ok(r.bpm)
    }

    /// Publish `plugin.<id>.<name>` with `data`.
    pub async fn emit(&self, name: &str, data: &impl Serialize) -> Result<()> {
        self.unary::<_, pb::Empty>("Emit", pb::EmitRequest { name: name.into(), data_json: serde_json::to_vec(data)? }).await?;
        Ok(())
    }

    /// Log a line to the plugin's log (`luna capsule logs`): debug, info, warn or error.
    pub async fn log(&self, level: &str, msg: impl Into<String>) {
        let msg = msg.into();
        if self.unary::<_, pb::Empty>("Log", pb::LogRequest { level: level.into(), msg: msg.clone() }).await.is_err() {
            eprintln!("[{level}] {msg}");
        }
    }

    /// Run `f` once after `d`, with this context (so a reply to a simulated event stays simulated).
    pub fn after<F, Fut>(&self, d: Duration, f: F)
    where
        F: FnOnce(Ctx) -> Fut + Send + 'static,
        Fut: Future<Output = Result<()>> + Send + 'static,
    {
        let c = self.clone();
        tokio::spawn(async move {
            tokio::time::sleep(d).await;
            if let Err(e) = f(c.clone()).await {
                c.log("error", format!("timer: {e}")).await;
            }
        });
    }

    /// Call a link ("provider:name", declared with [`Bot::uses`]) through Luna's broker.
    pub async fn link_call(&self, link: &str, args: Value) -> Result<Value> {
        let mut links = self.shared.links.lock().await;
        if !links.contains_key(link) {
            let dir = std::env::var("CAPSULE_LINKS").unwrap_or_default();
            let conn = frames::Conn::connect(&format!("{dir}/{link}"))
                .await
                .map_err(|_| Error::LinkUnavailable(format!("{link}: not approved or not in [links].uses")))?;
            links.insert(link.into(), conn);
        }
        let conn = links.get_mut(link).expect("just inserted");
        let method = link.rsplit(':').next().unwrap_or_default();
        let sent = conn.send("call", 1, Some(json!({"method": method, "body": args}))).await;
        let reply = match sent {
            Ok(()) => tokio::time::timeout(Duration::from_secs(10), conn.recv()).await.ok().and_then(|r| r.ok()),
            Err(_) => None,
        };
        let Some(f) = reply else {
            links.remove(link);
            return Err(Error::LinkUnavailable(format!("{link}: the link dropped")));
        };
        let b = f.b.unwrap_or(Value::Null);
        if b.get("ok").and_then(Value::as_bool) == Some(true) {
            return Ok(b.get("body").cloned().unwrap_or(Value::Null));
        }
        let err = b.get("error").and_then(Value::as_str).unwrap_or("failed").to_string();
        Err(match err.as_str() {
            "link_unavailable" | "timeout" => Error::LinkUnavailable(format!("{link}: {err}")),
            _ => Error::Other(format!("{link}: {err}")),
        })
    }
}

/// Chat through Luna's output guard; needs `[permissions].chat`.
pub struct Chat<'a>(&'a Ctx);

impl Chat<'_> {
    async fn send_mode(&self, text: String, mode: &str, to: &str) -> Result<()> {
        let c = self.0;
        c.unary::<_, pb::Empty>("ChatSend", pb::ChatSendRequest { text, simulated: c.simulated(), mode: mode.into(),
            to_user_id: to.into(), ..Default::default() }).await?;
        Ok(())
    }
    pub async fn send(&self, text: impl Into<String>) -> Result<()> {
        self.send_mode(text.into(), "", "").await
    }
    pub async fn announce(&self, text: impl Into<String>) -> Result<()> {
        self.send_mode(text.into(), "announce", "").await
    }
    pub async fn whisper(&self, user_id: &str, text: impl Into<String>) -> Result<()> {
        self.send_mode(text.into(), "whisper", user_id).await
    }
}

/// Moderation as the channel; needs the action in `[permissions].mod`.
pub struct Mod<'a>(&'a Ctx);

impl Mod<'_> {
    async fn act(&self, mut r: pb::ModRequest) -> Result<()> {
        r.simulated = self.0.simulated();
        self.0.unary::<_, pb::Empty>("Mod", r).await?;
        Ok(())
    }
    pub async fn timeout(&self, user_id: &str, seconds: i64, reason: &str) -> Result<()> {
        self.act(pb::ModRequest { action: "timeout".into(), user_id: user_id.into(), seconds, reason: reason.into(), ..Default::default() }).await
    }
    pub async fn ban(&self, user_id: &str, reason: &str) -> Result<()> {
        self.act(pb::ModRequest { action: "ban".into(), user_id: user_id.into(), reason: reason.into(), ..Default::default() }).await
    }
    pub async fn unban(&self, user_id: &str) -> Result<()> {
        self.act(pb::ModRequest { action: "unban".into(), user_id: user_id.into(), ..Default::default() }).await
    }
    pub async fn delete(&self, message_id: &str) -> Result<()> {
        self.act(pb::ModRequest { action: "delete".into(), message_id: message_id.into(), ..Default::default() }).await
    }
}

/// The plugin's own saved data in Luna's database; needs `store = true`.
pub struct Store<'a>(&'a Ctx);

impl Store<'_> {
    pub async fn get<T: DeserializeOwned>(&self, key: &str) -> Result<Option<T>> {
        let r: pb::StoreValue = self.0.unary("StoreGet", pb::StoreKey { key: key.into() }).await?;
        if !r.found {
            return Ok(None);
        }
        Ok(Some(serde_json::from_slice(&r.value_json)?))
    }
    /// Save a value; `ttl` None keeps it until deleted.
    pub async fn set(&self, key: &str, value: &impl Serialize, ttl: Option<Duration>) -> Result<()> {
        let ttl_ms = ttl.map_or(0, |d| d.as_millis() as i64);
        self.0.unary::<_, pb::Empty>("StoreSet", pb::StoreSetRequest { key: key.into(), value_json: serde_json::to_vec(value)?, ttl_ms }).await?;
        Ok(())
    }
    pub async fn delete(&self, key: &str) -> Result<()> {
        self.0.unary::<_, pb::Empty>("StoreDelete", pb::StoreKey { key: key.into() }).await?;
        Ok(())
    }
    pub async fn incr(&self, key: &str, by: i64) -> Result<i64> {
        let r: pb::StoreIncrReply = self.0.unary("StoreIncr", pb::StoreIncrRequest { key: key.into(), by }).await?;
        Ok(r.value)
    }
    pub async fn keys(&self, prefix: &str) -> Result<Vec<String>> {
        let r: pb::StoreKeysReply = self.0.unary("StoreKeys", pb::StoreKeysRequest { prefix: prefix.into() }).await?;
        Ok(r.keys)
    }
}

/// Twitch through Luna: reads need `"read"` in `[permissions].twitch`, each write its own
/// permission. Calls and their arguments are in api/plugin.proto (TwitchRequest).
pub struct Twitch<'a>(&'a Ctx);

impl Twitch<'_> {
    pub async fn call(&self, call: &str, user: &str, args: Option<Value>) -> Result<Value> {
        let args_json = args.map(|a| serde_json::to_vec(&a)).transpose()?.unwrap_or_default();
        let r: pb::DataReply = self.0.unary("Twitch", pb::TwitchRequest { call: call.into(), user: user.into(), args_json,
            simulated: self.0.simulated() }).await?;
        Ok(serde_json::from_slice(&r.data_json).unwrap_or(Value::Null))
    }
    /// A user by login or ID (Null if there's no such user).
    pub async fn user(&self, login_or_id: &str) -> Result<Value> {
        self.call("user", login_or_id.trim_start_matches('@'), None).await
    }
    /// A channel's stream ("" for this one): live, game, title, viewers.
    pub async fn stream(&self, id: &str) -> Result<Value> {
        self.call("stream", id, None).await
    }
}

/// VRChat through Luna; needs `[permissions].osc`.
pub struct Osc<'a>(&'a Ctx);

impl Osc<'_> {
    /// Set an avatar parameter (bool, number or string; numbers take the avatar's own type).
    pub async fn set_param(&self, name: &str, value: Value) -> Result<()> {
        let typ = match &value {
            Value::Bool(_) => "bool",
            Value::String(_) => "string",
            _ => "",
        };
        self.0.unary::<_, pb::DataReply>("Osc", pb::OscRequest { action: "send".into(), address: format!("/avatar/parameters/{name}"),
            value_json: serde_json::to_vec(&value)?, r#type: typ.into(), simulated: self.0.simulated(), ..Default::default() }).await?;
        Ok(())
    }
    /// Show text in the VRChat chatbox.
    pub async fn chatbox(&self, text: impl Into<String>) -> Result<()> {
        self.0.unary::<_, pb::DataReply>("Osc", pb::OscRequest { action: "chatbox".into(), text: text.into(),
            simulated: self.0.simulated(), ..Default::default() }).await?;
        Ok(())
    }
}

fn matches(pattern: &str, typ: &str) -> bool {
    match pattern.strip_suffix('*') {
        Some(p) => typ.starts_with(p),
        None => pattern == typ,
    }
}

// The supervisor restarts the capsule.
fn die(msg: &str) -> ! {
    eprintln!("{msg}");
    std::process::exit(1)
}

async fn live(bot: Bot) -> std::result::Result<(), String> {
    let mut ctl = frames::Conn::connect(&std::env::var("CAPSULE_CONTROL").unwrap_or_default()).await.map_err(|e| e.to_string())?;
    ctl.send("hello", 0, Some(json!({"spec": "0.1", "pid": std::process::id()}))).await.map_err(|e| e.to_string())?;
    if ctl.recv().await.map_err(|e| e.to_string())?.t != "welcome" {
        return Err("the runtime didn't welcome this capsule".into());
    }
    let sock = std::env::var("CAPSULE_HOST").unwrap_or_default();
    let channel = Endpoint::try_from("http://[::]:50051")
        .map_err(|e| e.to_string())?
        .connect_with_connector(tower::service_fn(move |_: tonic::transport::Uri| {
            let sock = sock.clone();
            async move { Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(tokio::net::UnixStream::connect(sock).await?)) }
        }))
        .await
        .map_err(|e| format!("can't reach Luna's plugin API: {e}"))?;
    let mut host = tonic::client::Grpc::new(channel);
    host.ready().await.map_err(|e| e.to_string())?;
    let hello: pb::HelloReply = host
        .unary(tonic::Request::new(pb::HelloRequest { api: API.into(), sdk: SDK.into() }),
            PathAndQuery::from_static("/luna.plugin.v1.Host/Hello"), tonic_prost::ProstCodec::default())
        .await
        .map_err(|e| format!("Luna refused the plugin API: {}", e.message()))?
        .into_inner();
    let mut config = bot.config.clone();
    if let Ok(Value::Object(m)) = serde_json::from_slice::<Value>(&hello.config_json) {
        config.extend(m);
    }
    let shared = Arc::new(Shared { host: host.clone(), id: hello.id, dev: hello.dev, config: Value::Object(config),
        links: Mutex::new(HashMap::new()) });
    let ctx = |event: Option<Event>| Ctx { shared: shared.clone(), event };

    // Each handler gets its own ordered queue, so a slow one doesn't hold up the others.
    let mut queues = Vec::new();
    for (pattern, f) in bot.handlers {
        let (tx, mut rx) = mpsc::channel::<Event>(1000);
        let c = ctx(None);
        tokio::spawn(async move {
            while let Some(ev) = rx.recv().await {
                let cx = Ctx { event: Some(ev.clone()), ..c.clone() };
                let typ = ev.r#type.clone();
                if let Err(e) = f(cx.clone(), ev).await {
                    cx.log("error", format!("handling {typ}: {e}")).await;
                }
            }
        });
        queues.push((pattern, tx));
    }
    if !queues.is_empty() {
        let mut types: Vec<String> = queues.iter().map(|(p, _)| p.clone()).collect();
        types.sort();
        types.dedup();
        let mut h = host.clone();
        // Luna sends a stream's headers with its first message, so awaiting the call here would stall ready.
        tokio::spawn(async move {
            let _ = h.ready().await;
            let mut stream = match h
                .server_streaming(tonic::Request::new(pb::SubscribeRequest { types }),
                    PathAndQuery::from_static("/luna.plugin.v1.Host/Subscribe"), tonic_prost::ProstCodec::<pb::SubscribeRequest, pb::Event>::default())
                .await
            {
                Ok(r) => r.into_inner(),
                Err(e) => die(&format!("subscribe: {}", e.message())),
            };
            loop {
                match stream.message().await {
                    Ok(Some(e)) => {
                        let ev = Event::from_pb(e);
                        for (p, tx) in &queues {
                            if matches(p, &ev.r#type) {
                                let _ = tx.try_send(ev.clone());
                            }
                        }
                    }
                    _ => die("event stream ended"),
                }
            }
        });
    }
    if !bot.chat.is_empty() || !bot.cli.is_empty() {
        let chat: HashMap<String, ChatFn> = bot.chat.into_iter().map(|(c, f)| (c.name, f)).collect();
        let cli: HashMap<String, CliFn> = bot.cli.into_iter().map(|(c, f)| (c.name, f)).collect();
        let (out_tx, out_rx) = mpsc::channel::<pb::CommandResult>(64);
        let mut h = host.clone();
        let base = ctx(None);
        tokio::spawn(async move {
            let _ = h.ready().await;
            let mut incoming = match h
                .streaming(tonic::Request::new(tokio_stream::wrappers::ReceiverStream::new(out_rx)),
                    PathAndQuery::from_static("/luna.plugin.v1.Host/Commands"),
                    tonic_prost::ProstCodec::<pb::CommandResult, pb::Command>::default())
                .await
            {
                Ok(r) => r.into_inner(),
                Err(e) => die(&format!("commands: {}", e.message())),
            };
            loop {
                let Ok(Some(c)) = incoming.message().await else { die("command stream ended") };
                let (chat, cli, out_tx, base) = (chat.clone(), cli.clone(), out_tx.clone(), base.clone());
                tokio::spawn(async move {
                    let cx = Ctx { event: c.event.map(Event::from_pb), ..base };
                    let args: Map<String, Value> = serde_json::from_slice(&c.args_json).unwrap_or_default();
                    let mut r = pb::CommandResult { id: c.id, ..Default::default() };
                    let res = if c.kind == "cli" {
                        match cli.get(&c.name) {
                            Some(f) => f(cx, args).await,
                            None => Err(Error::Other(format!("no CLI command {:?}", c.name))),
                        }
                    } else {
                        let words = args.get("args").and_then(|v| serde_json::from_value::<Vec<String>>(v.clone()).ok()).unwrap_or_default();
                        match chat.get(&c.name) {
                            Some(f) => f(cx, words).await.map(|_| Value::Null),
                            None => Err(Error::Other(format!("no chat command {:?}", c.name))),
                        }
                    };
                    match res {
                        Ok(Value::String(s)) => r.text = s,
                        Ok(Value::Null) => {}
                        Ok(v) => r.data_json = serde_json::to_vec(&v).unwrap_or_default(),
                        Err(e) => r.error = e.to_string(),
                    }
                    let _ = out_tx.send(r).await;
                });
            }
        });
    }
    for (d, f) in bot.every {
        let c = ctx(None);
        tokio::spawn(async move {
            let mut t = tokio::time::interval_at(tokio::time::Instant::now() + d, d);
            loop {
                t.tick().await;
                if let Err(e) = f(c.clone()).await {
                    c.log("error", format!("timer: {e}")).await;
                }
            }
        });
    }
    for f in &bot.on_start {
        let c = ctx(None);
        if let Err(e) = f(c.clone()).await {
            c.log("error", format!("on_start: {e}")).await;
        }
    }
    ctl.send("ready", 0, None).await.map_err(|e| e.to_string())?;

    let provides: HashMap<String, ProvideFn> = bot.provides.into_iter().collect();
    let (res_tx, mut res_rx) = mpsc::channel::<(u64, Value)>(64);
    loop {
        tokio::select! {
            f = ctl.recv() => {
                let Ok(f) = f else { break };
                match f.t.as_str() {
                    "shutdown" => break,
                    "ping" => { let _ = ctl.send("pong", f.id.unwrap_or(0), None).await; }
                    "call" => {
                        let b = f.b.unwrap_or(Value::Null);
                        let link = b.get("link").and_then(Value::as_str).unwrap_or_default().to_string();
                        let caller = b.get("caller").and_then(Value::as_str).unwrap_or_default().to_string();
                        let body = b.get("body").cloned().unwrap_or(Value::Null);
                        let (id, tx, c, p) = (f.id.unwrap_or(0), res_tx.clone(), ctx(None), provides.get(&link).cloned());
                        tokio::spawn(async move {
                            let r = match p {
                                None => json!({"ok": false, "error": "no such link"}),
                                Some(p) => match p(c, caller, body).await {
                                    Ok(v) => json!({"ok": true, "body": v}),
                                    Err(e) => json!({"ok": false, "error": e.to_string()}),
                                },
                            };
                            let _ = tx.send((id, r)).await;
                        });
                    }
                    _ => {}
                }
            }
            Some((id, r)) = res_rx.recv() => { let _ = ctl.send("result", id, Some(r)).await; }
        }
    }
    Ok(())
}
