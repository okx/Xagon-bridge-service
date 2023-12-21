package messagepush

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"os"

	"github.com/0xPolygonHermez/zkevm-bridge-service/bridgectrl/pb"
	"github.com/0xPolygonHermez/zkevm-bridge-service/utils"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/IBM/sarama"
	"github.com/pkg/errors"
)

type produceOptions struct {
	topic   string
	pushKey string
}

type produceOptFunc func(opts *produceOptions)

func WithTopic(topic string) produceOptFunc {
	return func(opts *produceOptions) {
		opts.topic = topic
	}
}

func WithPushKey(key string) produceOptFunc {
	return func(opts *produceOptions) {
		opts.pushKey = key
	}
}

type KafkaProducer interface {
	Produce(msg interface{}, optFns ...produceOptFunc) error
	PushTransactionUpdate(tx *pb.Transaction, optFns ...produceOptFunc) error
	Close() error
}

type kafkaProducerImpl struct {
	producer       sarama.SyncProducer
	defaultTopic   string
	defaultPushKey string
}

func NewKafkaProducer(cfg Config) (KafkaProducer, error) {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true

	// Enable SASL authentication
	if cfg.Username != "" && cfg.Password != "" && cfg.RootCAPath != "" {
		config.Net.SASL.Enable = true
		config.Net.SASL.User = cfg.Username
		config.Net.SASL.Password = cfg.Password

		// Read the CA cert from file
		rootCA, err := os.ReadFile(cfg.RootCAPath)
		if err != nil {
			return nil, errors.Wrap(err, "NewKafkaProducer read root CA cert fail")
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM([]byte(rootCA)); !ok {
			return nil, errors.New("NewKafkaProducer caCertPool.AppendCertsFromPEM")
		}

		config.Net.TLS.Enable = true
		config.Net.TLS.Config = &tls.Config{RootCAs: caCertPool, InsecureSkipVerify: true} // #nosec
	}

	producer, err := sarama.NewSyncProducer(cfg.Brokers, config)
	if err != nil {
		return nil, errors.Wrap(err, "NewKafkaProducer: NewSyncProducer error")
	}
	return &kafkaProducerImpl{
		producer:       producer,
		defaultTopic:   cfg.Topic,
		defaultPushKey: cfg.PushKey,
	}, nil
}

// Produce send a message to the Kafka topic
// msg should be either a string or an object
// If msg is an object, it will be encoded to JSON before being sent
func (p *kafkaProducerImpl) Produce(msg interface{}, optFns ...produceOptFunc) error {
	if p == nil || p.producer == nil {
		log.Debugf("Kafka producer is nil")
		return nil
	}
	opts := &produceOptions{
		topic:   p.defaultTopic,
		pushKey: p.defaultPushKey,
	}
	for _, f := range optFns {
		f(opts)
	}

	var msgString string
	switch v := msg.(type) {
	case string:
		// If message is a string, just send it
		msgString = v
	default:
		// If message is an object, encode to json
		b, err := json.Marshal(msg)
		if err != nil {
			log.Errorf("msg cannot be encoded to json: msg[%v] err[%v]", msg, err)
			return errors.Wrap(err, "kafka produce: JSON marshal error")
		}
		msgString = string(b)
	}

	produceMsg := &sarama.ProducerMessage{
		Topic: opts.topic,
		Value: sarama.StringEncoder(msgString),
	}
	if opts.pushKey != "" {
		produceMsg.Key = sarama.StringEncoder(opts.pushKey)
	}

	// Send message to the topic
	partition, offset, err := p.producer.SendMessage(produceMsg)

	if err != nil {
		return errors.Wrap(err, "kafka SendMessage error")
	}

	log.Debugf("Produced to Kafka: topic[%v] msg[%v] partition[%v] offset[%v]", opts.topic, msgString, partition, offset)
	return nil
}

func (p *kafkaProducerImpl) PushTransactionUpdate(tx *pb.Transaction, optFns ...produceOptFunc) error {
	b, err := json.Marshal(tx)
	if err != nil {
		return errors.Wrap(err, "json marshal error")
	}

	msg := &PushMessage{
		BizCode:       BizCodeBridgeOrder,
		WalletAddress: tx.GetDestAddr(),
		RequestID:     utils.GenerateTraceID(),
		PushContent:   string(b),
	}

	return p.Produce(msg, optFns...)
}

func (p *kafkaProducerImpl) Close() error {
	return p.producer.Close()
}
