"""What the Python runtime starts: put the SDK, the runtime's packages and the plugin's
own vendor/ on the path, then run the plugin's entry as __main__."""

import runpy
import sys

entry = sys.argv[1]
sys.argv = sys.argv[1:]
sys.path[:0] = ["/sdk", "/runtime/site", "/pkg/vendor", "/pkg"]
runpy.run_path(entry, run_name="__main__")
