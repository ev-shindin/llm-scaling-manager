package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("kueue.controllerNamespace", func() {
	validate := func(ns string) error {
		var p QuotaLimiterEntries
		src := "limiters:\n- name: ns\n  type: quota\n  scope: namespace\n" +
			"  kueue:\n    enabled: true\n    controllerNamespace: " + ns + "\n"
		Expect(yaml.Unmarshal([]byte(src), &p)).To(Succeed())
		_, err := p.Validate()
		return err
	}

	It("accepts a namespace name and rejects anything else", func() {
		Expect(validate("queueing")).To(Succeed())
		Expect(validate("Kueue_System")).To(MatchError(ContainSubstring("kueue.controllerNamespace")))
	})
})
