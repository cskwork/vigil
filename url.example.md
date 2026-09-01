# vigil targets
#
# One deployed URL per line. The first URL is the primary target:
#   - its origin becomes target.base_url (and its host is auto-allowlisted)
#   - its path becomes the default entry route for seed scenarios and agent discovery
# Format: <url>  or  <label> | <url>. Lines starting with # are ignored.
# Copy this file to url.md (git-ignored) and replace the URL.

home | https://dev.example.com/
