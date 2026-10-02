#!/usr/bin/env python3
"""Vendor the subset of GitHub's public GraphQL schema the fake resolves.

Source: GitHub's public schema (the docs SDL, schema.docs.graphql, as
published at https://docs.github.com/public/fpt/schema.docs.graphql), cached
locally; NEVER fetched by introspection. Run:

    python3 extract_graphql.py ~/.claude/cache/github-graphql-schema.docs.graphql > graphql-subset.graphql

Each named definition is copied with its description strings and directives
stripped; field lists, arguments and types are unchanged.
"""
import re, sys

NAMES = """
Query Mutation Node Actor User Bot Mannequin Organization EnterpriseUserAccount
Repository PullRequest Issue Label LabelConnection UserConnection
PullRequestReview PullRequestReviewConnection PullRequestReviewState PullRequestReviewDecision
PullRequestReviewThread PullRequestReviewThreadConnection
PullRequestReviewComment PullRequestReviewCommentConnection MergeStateStatus
ProjectV2Item ProjectV2ItemConnection ProjectV2ItemContent ProjectV2ItemFieldValue
ProjectV2ItemFieldValueConnection ProjectV2ItemFieldSingleSelectValue ProjectV2ItemFieldTextValue
ProjectV2FieldCommon ProjectV2Field ProjectV2SingleSelectField ProjectV2IterationField ProjectV2FieldConfiguration
LinkedBranchConnection PullRequestConnection
AddReactionInput AddReactionPayload RemoveReactionInput RemoveReactionPayload Reaction ReactionContent
MarkPullRequestReadyForReviewInput MarkPullRequestReadyForReviewPayload
ConvertPullRequestToDraftInput ConvertPullRequestToDraftPayload
""".split()

def main(path):
    lines = open(path).read().split("\n")
    out = []
    for name in NAMES:
        start = None
        for i, l in enumerate(lines):
            if re.match(r"^(type|interface|union|enum|input|scalar) %s\b" % re.escape(name), l):
                start = i
                break
        if start is None:
            sys.exit("missing definition: " + name)
        block = [lines[start]]
        i = start + 1
        if lines[start].startswith("union"):
            while i < len(lines) and lines[i].strip().startswith("|"):
                block.append(lines[i]); i += 1
        elif not lines[start].rstrip().endswith("}"):
            depth = lines[start].count("{") - lines[start].count("}")
            while i < len(lines) and (depth > 0 or "{" not in "".join(block)):
                block.append(lines[i])
                depth += lines[i].count("{") - lines[i].count("}")
                i += 1
                if depth == 0 and "{" in "".join(block):
                    break
        text = "\n".join(block)
        text = re.sub(r'"""(.|\n)*?"""', "", text)     # block descriptions
        text = re.sub(r'(?m)^\s*"[^"\n]*"\s*$', "", text)  # line descriptions
        text = re.sub(r'\s@\w+(\((?:[^()"]|"(?:[^"\\]|\\.)*")*\))?', "", text, flags=re.S)  # directives (args may span lines)
        text = "\n".join(l for l in text.split("\n") if l.strip())
        out.append(text)
    print("# Vendored subset of GitHub's public GraphQL schema (see extract_graphql.py).\n")
    print("\n\n".join(out))

main(sys.argv[1])
