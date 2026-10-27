package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
)

// splitRepoTagRef splits "repo[:tag]" (a registry port is not a tag).
func splitRepoTagRef(s string) (string, string) {
	if i := strings.LastIndexByte(s, ':'); i > strings.LastIndexByte(s, '/') {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func commitCmd(client clientFunc) *cobra.Command {
	var author, message string
	var changes []string
	cmd := &cobra.Command{
		Use:   "commit [OPTIONS] CONTAINER [REPOSITORY[:TAG]]",
		Short: "Create an image from a container's changes",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			o := api.CommitOptions{Container: args[0], Author: author, Comment: message, Changes: changes}
			if len(args) == 2 {
				o.Repo, o.Tag = splitRepoTagRef(args[1])
			}
			id, err := c.Commit(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&author, "author", "a", "", "author (e.g. \"Jane <jane@example.org>\")")
	f.StringVarP(&message, "message", "m", "", "commit message")
	f.StringArrayVarP(&changes, "change", "c", nil, "apply a Dockerfile instruction (CMD, ENTRYPOINT, ENV, EXPOSE, LABEL, ONBUILD, STOPSIGNAL, USER, VOLUME, WORKDIR)")
	return cmd
}

func tagCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "tag SOURCE_IMAGE[:TAG] TARGET_IMAGE[:TAG]",
		Short: "Give an image another name",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			repo, tag := splitRepoTagRef(args[1])
			return c.ImageTag(cmd.Context(), args[0], repo, tag)
		},
	}
}
