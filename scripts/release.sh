#!/usr/bin/env bash
#
# release.sh — Create a versioned release tag for Call-VPN.
#
# Usage:
#   ./scripts/release.sh [--patch|--minor|--major] [--dry-run] [--help]
#   ./scripts/release.sh v1.2.0
#
# Compatible with Linux, macOS, Git Bash (MINGW).

set -euo pipefail

GRADLE_FILE="mobile/android/app/build.gradle.kts"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS] [VERSION]

Create an annotated git tag and push it to trigger the release workflow.

Options:
  --patch       Bump patch version (default)
  --minor       Bump minor version
  --major       Bump major version
  --dry-run     Show what would be done without executing
  --help        Show this help message

Arguments:
  VERSION       Explicit version (e.g., v1.2.0). Overrides auto-increment.

Examples:
  $(basename "$0")              # auto-increment patch from last tag
  $(basename "$0") --minor      # auto-increment minor
  $(basename "$0") v2.0.0       # explicit version
  $(basename "$0") --dry-run    # preview without changes
EOF
    exit 0
}

die() { echo "Error: $*" >&2; exit 1; }

# Parse arguments
BUMP="patch"
DRY_RUN=false
EXPLICIT_VERSION=""

for arg in "$@"; do
    case "$arg" in
        --patch) BUMP="patch" ;;
        --minor) BUMP="minor" ;;
        --major) BUMP="major" ;;
        --dry-run) DRY_RUN=true ;;
        --help|-h) usage ;;
        v[0-9]*) EXPLICIT_VERSION="$arg" ;;
        *) die "Unknown argument: $arg" ;;
    esac
done

# Must be in a git repo
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "Not a git repository"

# Check for uncommitted changes
if [ -n "$(git status --porcelain)" ]; then
    echo "Uncommitted changes detected:"
    git status --short
    die "Commit or stash your changes before releasing."
fi

# Determine current version from latest tag
LAST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
echo "Last tag: $LAST_TAG"

# Parse semver components
VERSION_BODY="${LAST_TAG#v}"
IFS='.' read -r MAJOR MINOR PATCH <<< "$VERSION_BODY"
MAJOR="${MAJOR:-0}"
MINOR="${MINOR:-0}"
PATCH="${PATCH:-0}"

# Calculate new version
if [ -n "$EXPLICIT_VERSION" ]; then
    NEW_TAG="$EXPLICIT_VERSION"
else
    case "$BUMP" in
        major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
        minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
        patch) PATCH=$((PATCH + 1)) ;;
    esac
    NEW_TAG="v${MAJOR}.${MINOR}.${PATCH}"
fi

NEW_VERSION="${NEW_TAG#v}"
IFS='.' read -r NEW_MAJOR NEW_MINOR NEW_PATCH <<< "$NEW_VERSION"

echo "New version: $NEW_TAG"
echo ""

# Show unpushed commits
UPSTREAM=$(git rev-parse --abbrev-ref '@{upstream}' 2>/dev/null || echo "")
if [ -n "$UPSTREAM" ]; then
    UNPUSHED=$(git log "$UPSTREAM..HEAD" --oneline 2>/dev/null || echo "")
    if [ -n "$UNPUSHED" ]; then
        echo "Unpushed commits:"
        echo "$UNPUSHED"
        echo ""
    fi
fi

if $DRY_RUN; then
    echo "[dry-run] Would update $GRADLE_FILE:"
    echo "  versionCode = <incremented>"
    echo "  versionName = \"$NEW_VERSION\""
    echo "[dry-run] Would commit version bump"
    echo "[dry-run] Would create annotated tag: $NEW_TAG"
    echo "[dry-run] Would push commits and tag"
    exit 0
fi

# Confirm
echo "This will:"
echo "  1. Update versionName/versionCode in $GRADLE_FILE"
echo "  2. Commit the version bump"
echo "  3. Create annotated tag $NEW_TAG"
echo "  4. Push commits and tag to origin"
echo ""
read -r -p "Proceed? [y/N] " CONFIRM
case "$CONFIRM" in
    [yY]|[yY][eE][sS]) ;;
    *) echo "Aborted."; exit 1 ;;
esac

# Update build.gradle.kts
if [ -f "$GRADLE_FILE" ]; then
    # Read current versionCode and increment
    CURRENT_CODE=$(grep -oP 'versionCode\s*=\s*\K[0-9]+' "$GRADLE_FILE" || echo "1")
    NEW_CODE=$((CURRENT_CODE + 1))

    # Use sed compatible with both GNU and BSD (macOS)
    if sed --version >/dev/null 2>&1; then
        # GNU sed
        sed -i "s/versionCode = [0-9]*/versionCode = $NEW_CODE/" "$GRADLE_FILE"
        sed -i "s/versionName = \"[^\"]*\"/versionName = \"$NEW_VERSION\"/" "$GRADLE_FILE"
    else
        # BSD sed (macOS)
        sed -i '' "s/versionCode = [0-9]*/versionCode = $NEW_CODE/" "$GRADLE_FILE"
        sed -i '' "s/versionName = \"[^\"]*\"/versionName = \"$NEW_VERSION\"/" "$GRADLE_FILE"
    fi

    echo "Updated $GRADLE_FILE: versionCode=$NEW_CODE, versionName=$NEW_VERSION"
    git add "$GRADLE_FILE"
    git commit -m "chore: bump version to $NEW_TAG"
else
    echo "Warning: $GRADLE_FILE not found, skipping version bump in Gradle"
fi

# Create annotated tag
git tag -a "$NEW_TAG" -m "Release $NEW_TAG"
echo "Created tag: $NEW_TAG"

# Push
git push origin HEAD
git push origin "$NEW_TAG"

echo ""
echo "Release $NEW_TAG pushed. GitHub Actions will build and publish artifacts."
