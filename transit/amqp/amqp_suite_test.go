package amqp_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestAmqp(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Amqp Suite")
}
