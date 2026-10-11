"""Every error a plugin can get from Luna. Catch LunaError to catch them all."""


class LunaError(Exception):
    pass


class PermissionDenied(LunaError):
    """The manifest doesn't allow this call; the message names the line to add."""


class RateLimited(LunaError):
    def __init__(self, msg, retry_after=None):
        super().__init__(msg)
        self.retry_after = retry_after


class LinkUnavailable(LunaError):
    """The link's provider is restarting, failed or not installed."""


class ConfigError(LunaError):
    pass


class NotConnected(LunaError):
    pass


class Timeout(LunaError):
    pass
