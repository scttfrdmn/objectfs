#!/bin/bash
# Sync labels from labels.yml to GitHub repository

set -e

REPO="scttfrdmn/objectfs"
LABELS_FILE=".github/labels.yml"

echo "🏷️  Syncing labels to GitHub repository: $REPO"
echo

# Parse YAML and create labels
# This is a simple parser that works with our specific YAML format
current_name=""
current_desc=""
current_color=""

while IFS= read -r line; do
    if [[ $line =~ ^-\ name:\ \"(.+)\" ]]; then
        # If we have a previous label, create it
        if [[ -n "$current_name" ]]; then
            echo "Creating label: $current_name"
            gh api repos/$REPO/labels \
                -f name="$current_name" \
                -f description="$current_desc" \
                -f color="$current_color" \
                --silent 2>/dev/null || \
            gh api repos/$REPO/labels/$current_name \
                -X PATCH \
                -f description="$current_desc" \
                -f color="$current_color" \
                --silent 2>/dev/null || \
            echo "  ⚠️  Failed to create/update: $current_name"
        fi

        # Start new label
        current_name="${BASH_REMATCH[1]}"
        current_desc=""
        current_color=""
    elif [[ $line =~ ^\ \ description:\ \"(.+)\" ]]; then
        current_desc="${BASH_REMATCH[1]}"
    elif [[ $line =~ ^\ \ color:\ \"(.+)\" ]]; then
        current_color="${BASH_REMATCH[1]}"
    fi
done < "$LABELS_FILE"

# Create the last label
if [[ -n "$current_name" ]]; then
    echo "Creating label: $current_name"
    gh api repos/$REPO/labels \
        -f name="$current_name" \
        -f description="$current_desc" \
        -f color="$current_color" \
        --silent 2>/dev/null || \
    gh api repos/$REPO/labels/$current_name \
        -X PATCH \
        -f description="$current_desc" \
        -f color="$current_color" \
        --silent 2>/dev/null || \
    echo "  ⚠️  Failed to create/update: $current_name"
fi

echo
echo "✅ Label sync complete!"
