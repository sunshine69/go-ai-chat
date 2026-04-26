package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
)

func HTMLToMarkdown(html string) (string, error) {
	if strings.TrimSpace(html) == "" {
		return "", nil
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("html to md: %w", err)
	}

	return strings.TrimSpace(md), nil
}

func FetchAndConvertKernelOrg() (string, error) {
	resp, err := http.Get("https://kernel.org/")
	if err != nil {
		return "", fmt.Errorf("fetch kernel.org: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	return HTMLToMarkdown(string(body))
}

func main() {
	o, err := FetchAndConvertKernelOrg()
	if err != nil {
		println(err.Error())

	} else {
		println(o)
	}

}
