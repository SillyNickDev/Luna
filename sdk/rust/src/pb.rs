//! The plugin API's messages (api/plugin.proto), written out so plugin authors need no protoc.

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct Empty {}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct HelloRequest {
    #[prost(string, tag = "1")]
    pub api: String,
    #[prost(string, tag = "2")]
    pub sdk: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct HelloReply {
    #[prost(string, tag = "1")]
    pub id: String,
    #[prost(bytes = "vec", tag = "2")]
    pub config_json: Vec<u8>,
    #[prost(bool, tag = "3")]
    pub dev: bool,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct SubscribeRequest {
    #[prost(string, repeated, tag = "1")]
    pub types: Vec<String>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct Event {
    #[prost(string, tag = "1")]
    pub id: String,
    #[prost(string, tag = "2")]
    pub r#type: String,
    #[prost(string, tag = "3")]
    pub source: String,
    #[prost(int64, tag = "4")]
    pub ts_ms: i64,
    #[prost(bool, tag = "5")]
    pub simulated: bool,
    #[prost(bytes = "vec", tag = "6")]
    pub data_json: Vec<u8>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct Command {
    #[prost(uint64, tag = "1")]
    pub id: u64,
    #[prost(string, tag = "2")]
    pub kind: String,
    #[prost(string, tag = "3")]
    pub name: String,
    #[prost(bytes = "vec", tag = "4")]
    pub args_json: Vec<u8>,
    #[prost(message, optional, tag = "5")]
    pub event: Option<Event>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct CommandResult {
    #[prost(uint64, tag = "1")]
    pub id: u64,
    #[prost(bytes = "vec", tag = "2")]
    pub data_json: Vec<u8>,
    #[prost(string, tag = "3")]
    pub text: String,
    #[prost(string, tag = "4")]
    pub error: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct ChatSendRequest {
    #[prost(string, tag = "1")]
    pub text: String,
    #[prost(bool, tag = "2")]
    pub simulated: bool,
    #[prost(string, tag = "3")]
    pub reply_to: String,
    #[prost(string, tag = "4")]
    pub mode: String,
    #[prost(string, tag = "5")]
    pub to_user_id: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct ModRequest {
    #[prost(string, tag = "1")]
    pub action: String,
    #[prost(string, tag = "2")]
    pub user_id: String,
    #[prost(int64, tag = "3")]
    pub seconds: i64,
    #[prost(string, tag = "4")]
    pub reason: String,
    #[prost(string, tag = "5")]
    pub message_id: String,
    #[prost(bool, tag = "6")]
    pub on: bool,
    #[prost(bool, tag = "7")]
    pub simulated: bool,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct TwitchRequest {
    #[prost(string, tag = "1")]
    pub call: String,
    #[prost(string, tag = "2")]
    pub user: String,
    #[prost(bytes = "vec", tag = "3")]
    pub args_json: Vec<u8>,
    #[prost(bool, tag = "4")]
    pub simulated: bool,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct DataReply {
    #[prost(bytes = "vec", tag = "1")]
    pub data_json: Vec<u8>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct ViewerRequest {
    #[prost(string, tag = "1")]
    pub user: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreKey {
    #[prost(string, tag = "1")]
    pub key: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreValue {
    #[prost(bool, tag = "1")]
    pub found: bool,
    #[prost(bytes = "vec", tag = "2")]
    pub value_json: Vec<u8>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreSetRequest {
    #[prost(string, tag = "1")]
    pub key: String,
    #[prost(bytes = "vec", tag = "2")]
    pub value_json: Vec<u8>,
    #[prost(int64, tag = "3")]
    pub ttl_ms: i64,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreIncrRequest {
    #[prost(string, tag = "1")]
    pub key: String,
    #[prost(int64, tag = "2")]
    pub by: i64,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreIncrReply {
    #[prost(int64, tag = "1")]
    pub value: i64,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreKeysRequest {
    #[prost(string, tag = "1")]
    pub prefix: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct StoreKeysReply {
    #[prost(string, repeated, tag = "1")]
    pub keys: Vec<String>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct LogRequest {
    #[prost(string, tag = "1")]
    pub level: String,
    #[prost(string, tag = "2")]
    pub msg: String,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct EmitRequest {
    #[prost(string, tag = "1")]
    pub name: String,
    #[prost(bytes = "vec", tag = "2")]
    pub data_json: Vec<u8>,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct OscRequest {
    #[prost(string, tag = "1")]
    pub action: String,
    #[prost(string, tag = "2")]
    pub address: String,
    #[prost(bytes = "vec", tag = "3")]
    pub value_json: Vec<u8>,
    #[prost(string, tag = "4")]
    pub r#type: String,
    #[prost(string, tag = "5")]
    pub text: String,
    #[prost(string, tag = "6")]
    pub name: String,
    #[prost(bool, tag = "7")]
    pub simulated: bool,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct HrRequest {
    #[prost(int32, tag = "1")]
    pub minutes: i32,
}

#[derive(Clone, PartialEq, ::prost::Message)]
pub struct HrReply {
    #[prost(int32, tag = "1")]
    pub bpm: i32,
    #[prost(int64, tag = "2")]
    pub at_ms: i64,
}
