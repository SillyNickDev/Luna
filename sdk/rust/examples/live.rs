//! The live test's plugin (live_test.go): the same one the TypeScript SDK's test runs.
use luna::{Bot, CliCommand, serde_json::json};

#[derive(serde::Serialize, serde::Deserialize)]
struct Config {
    greeting: String,
}

impl Default for Config {
    fn default() -> Self {
        Config { greeting: "Welcome".into() }
    }
}

fn main() {
    let mut bot = Bot::new();
    bot.use_config::<Config>();
    bot.on_start(|ctx| async move { ctx.store().set("started", &json!({"at": 1, "big": 1u64 << 40, "neg": -5}), None).await });
    bot.on("twitch.channel.follow", |ctx, ev| async move {
        let n = ctx.store().incr("follows", 1).await?;
        let c: Config = ctx.config()?;
        ctx.chat().send(format!("{}, {}! #{n}", c.greeting, ev.user().unwrap_or_default().name)).await
    });
    bot.cli(CliCommand::new("count", "follows seen"), |ctx, _| async move {
        Ok(json!({"follows": ctx.store().get::<i64>("follows").await?.unwrap_or(0)}))
    });
    bot.provide("echo", |_, caller, body| async move { Ok(json!({"caller": caller, "body": body})) });
    bot.run();
}
