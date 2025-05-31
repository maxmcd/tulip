package main

import (
	"fmt"
	"net/smtp"
	"os"
	"strings"
)

func sendMail(to string, subject string, body string) error {
	smtpServer := os.Getenv("SMTP_HOST")
	fromEmail := os.Getenv("SMTP_EMAIL")
	password := os.Getenv("SMTP_PASSWORD")
	host, _, _ := strings.Cut(smtpServer, ":")

	return smtp.SendMail(smtpServer, smtp.PlainAuth("", fromEmail, password, host), fromEmail, []string{to}, []byte(fmt.Sprintf("Subject: %s\r\n%s\r\n", subject, body)))
}
