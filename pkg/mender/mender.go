package mender

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) NeedsCommit() (bool, error) {
	// cmd := exec.Command("mender-update", "show-artifact")
	// var stdout, stderr bytes.Buffer
	// cmd.Stdout = &stdout
	// cmd.Stderr = &stderr

	// err := cmd.Run()
	// if err != nil {
	// 	return false, fmt.Errorf("error running mender-update show-artifact: %w, stderr: %s", err, stderr.String())
	// }

	// output := stdout.String()
	// log.Printf("mender-update show-artifact output: %s", output)

	// return strings.Contains(output, "State: pending"), nil
	return true, nil
}

func (c *Client) Install(filePath string) error {
	log.Printf("Installing update from %s", filePath)
	cmd := exec.Command("mender-update", "install", filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error running mender-update install: %w, stderr: %s", err, stderr.String())
	}

	log.Printf("mender-update install output: %s", stdout.String())
	return nil
}

func (c *Client) Commit() error {
	log.Printf("Committing update")
	cmd := exec.Command("mender-update", "commit")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error running mender-update commit: %w, stderr: %s", err, stderr.String())
	}

	log.Printf("mender-update commit output: %s", stdout.String())
	return nil
}
